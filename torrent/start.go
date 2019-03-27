package torrent

import (
	"net"

	"github.com/cenkalti/rain/internal/acceptor"
	"github.com/cenkalti/rain/internal/allocator"
	"github.com/cenkalti/rain/internal/announcer"
	"github.com/cenkalti/rain/internal/peer"
	"github.com/cenkalti/rain/internal/piecedownloader"
	"github.com/cenkalti/rain/internal/piecepicker"
	"github.com/cenkalti/rain/internal/tracker"
	"github.com/cenkalti/rain/internal/urldownloader"
	"github.com/cenkalti/rain/internal/verifier"
	"github.com/cenkalti/rain/internal/webseedsource"
)

func (t *torrent) start() {
	// Do not start if already started.
	if t.errC != nil {
		return
	}

	// Stop announcing Stopped event if in "Stopping" state.
	if t.stoppedEventAnnouncer != nil {
		t.stoppedEventAnnouncer.Close()
		t.stoppedEventAnnouncer = nil
	}

	t.log.Info("starting torrent")
	t.errC = make(chan error, 1)
	t.portC = make(chan int, 1)
	t.lastError = nil

	if t.info != nil {
		if t.pieces != nil {
			if t.bitfield != nil {
				t.startAcceptor()
				t.startAnnouncers()
				t.startPieceDownloaders()
			} else {
				t.startVerifier()
			}
		} else {
			t.startAllocator()
		}
	} else {
		t.startAcceptor()
		t.startAnnouncers()
		t.startInfoDownloaders()
	}
}

func (t *torrent) startVerifier() {
	if t.verifier != nil {
		panic("verifier exists")
	}
	t.verifier = verifier.New()
	go t.verifier.Run(t.pieces, t.verifierProgressC, t.verifierResultC)
}

func (t *torrent) startAllocator() {
	if t.allocator != nil {
		panic("allocator exists")
	}
	t.allocator = allocator.New()
	go t.allocator.Run(t.info, t.storage, t.allocatorProgressC, t.allocatorResultC)
}

func (t *torrent) startAnnouncers() {
	if len(t.announcers) > 0 {
		return
	}
	for _, tr := range t.trackers {
		t.startNewAnnouncer(tr)
	}
	if t.dhtNode != nil && t.dhtAnnouncer == nil {
		t.dhtAnnouncer = announcer.NewDHTAnnouncer()
		go t.dhtAnnouncer.Run(t.dhtNode.Announce, t.config.DHTAnnounceInterval, t.config.DHTMinAnnounceInterval, t.log)
	}
}

func (t *torrent) startNewAnnouncer(tr tracker.Tracker) {
	an := announcer.NewPeriodicalAnnouncer(
		tr,
		t.config.TrackerNumWant,
		t.config.TrackerMinAnnounceInterval,
		t.announcerFields,
		t.completeC,
		t.addrsFromTrackers,
		t.log,
	)
	t.announcers = append(t.announcers, an)
	go an.Run()
}

func (t *torrent) startAcceptor() {
	if t.acceptor != nil {
		return
	}
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{Port: t.port})
	if err != nil {
		t.log.Warningf("cannot listen port %d: %s", t.port, err)
	} else {
		t.log.Info("Listening peers on tcp://" + listener.Addr().String())
		t.port = listener.Addr().(*net.TCPAddr).Port
		t.portC <- t.port
		t.acceptor = acceptor.New(listener, t.incomingConnC, t.log)
		go t.acceptor.Run()
	}
}

func (t *torrent) startInfoDownloaders() {
	if t.info != nil {
		return
	}
	for len(t.infoDownloaders)-len(t.infoDownloadersSnubbed) < t.config.ParallelMetadataDownloads {
		id := t.nextInfoDownload()
		if id == nil {
			break
		}
		t.infoDownloaders[id.Peer.(*peer.Peer)] = id
		id.RequestBlocks(t.config.RequestQueueLength)
		id.Peer.(*peer.Peer).ResetSnubTimer()
	}
}

func (t *torrent) startPieceDownloaders() {
	if t.status() != Downloading {
		return
	}
	for _, src := range t.webseedSources {
		if !src.Downloading() && !src.Disabled {
			started := t.startPieceDownloaderForWebseed(src)
			if !started {
				break
			}
		}
	}
	for pe := range t.peers {
		if !pe.Downloading {
			t.startPieceDownloaderFor(pe)
		}
	}
}

func (t *torrent) startPieceDownloaderForWebseed(src *webseedsource.WebseedSource) (started bool) {
	sp := t.piecePicker.PickWebseed(src)
	if sp == nil {
		return false
	}
	t.startWebseedDownloader(sp)
	return true
}

func (t *torrent) startWebseedDownloader(sp *piecepicker.WebseedDownloadSpec) {
	t.log.Debugf("downloading pieces %d-%d from webseed %s", sp.Begin, sp.End, sp.Source.URL)
	ud := urldownloader.New(sp.Source.URL, sp.Begin, sp.End)
	for _, src := range t.webseedSources {
		if src != sp.Source {
			continue
		}
		if src.Downloader != nil {
			panic("already downloading from same url source")
		}
		src.Downloader = ud
		src.Disabled = false
		src.LastError = nil
		break
	}
	go ud.Run(t.webseedClient, t.pieces, t.info.MultiFile(), t.webseedPieceResultC.SendC(), t.piecePool, &t.piecePicker.MutexWebseed, t.config.WebseedResponseBodyReadTimeout)
}

func (t *torrent) startPieceDownloaderFor(pe *peer.Peer) {
	if t.status() != Downloading {
		return
	}
	if t.ram == nil {
		t.startSinglePieceDownloader(pe)
		return
	}
	ok := t.ram.Request(string(t.peerID[:]), pe, int64(t.info.PieceLength), t.ramNotifyC, pe.Done())
	if ok {
		t.startSinglePieceDownloader(pe)
	}
}

func (t *torrent) startSinglePieceDownloader(pe *peer.Peer) {
	var started bool
	defer func() {
		if !started && t.ram != nil {
			t.ram.Release(int64(t.info.PieceLength))
		}
	}()
	if t.status() != Downloading {
		return
	}
	pi, allowedFast := t.piecePicker.PickFor(pe)
	if pi == nil {
		return
	}
	pd := piecedownloader.New(pi, pe, allowedFast, t.piecePool.Get(int(pi.Length)))
	if _, ok := t.pieceDownloaders[pe]; ok {
		panic("peer already has a piece downloader")
	}
	t.pieceDownloaders[pe] = pd
	pe.Downloading = true
	pd.RequestBlocks(t.config.RequestQueueLength)
	pe.ResetSnubTimer()
	started = true
}
