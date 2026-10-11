package domain

import "time"

// TorrentStatus is the state of one torrent in the client, reduced to what the
// project acts on.
//
// It is the boundary type between the qBittorrent client and everything else,
// so the qBittorrent version differences (5.x renamed the paused states) stop
// at the client implementation and never reach the workflow.
type TorrentStatus struct {
	Hash string
	Name string

	// State is the client's own state string, normalised to lower case but
	// otherwise passed through. Kept for logs and the read model; decisions
	// use the helpers below rather than comparing it directly.
	State string

	// Progress is 0.0 to 1.0.
	Progress float64
	// DlSpeed is bytes per second.
	DlSpeed int64
	// ETA is seconds as reported by the client. A client that does not know
	// reports a very large value; callers should treat anything above a day as
	// unknown.
	ETA int64
	// Size is the total size in bytes.
	Size int64

	SavePath    string
	ContentPath string
	Tags        []string

	// MaxProgress is the highest progress ever observed, tracked by the
	// poller across its own polls. It is what distinguishes a torrent that
	// never started from one that died part-way: an infrastructure failure
	// against a release worth blacklisting.
	MaxProgress float64

	// AddedAt is when the project handed the torrent to the client.
	AddedAt time.Time
}

// doneStates are the qBittorrent states that mean the payload is fully
// downloaded.
//
// qBittorrent 5.0 renamed pausedUP/pausedDL to stoppedUP/stoppedDL. Both
// spellings are listed so the project works against either, and so an upgrade
// of the client does not silently break completion detection.
//
// Anything ending in "UP" is a post-download state; "uploading" is the one
// that means actively seeding with peers.
var doneStates = map[string]bool{
	"uploading":  true,
	"stalledup":  true,
	"queuedup":   true,
	"checkingup": true,
	"forcedup":   true,
	"pausedup":   true, // qBittorrent 4.x
	"stoppedup":  true, // qBittorrent 5.x
}

// Done reports whether the download finished.
func (t TorrentStatus) Done() bool {
	if t.Progress >= 1.0 {
		return true
	}
	return doneStates[t.State]
}

// failedStates are the states that need no further waiting.
var failedStates = map[string]bool{
	"error":        true,
	"missingfiles": true,
}

// Failed reports whether the client has given up on the torrent.
func (t TorrentStatus) Failed() bool {
	return failedStates[t.State]
}

// Stalled reports whether the torrent is not making progress and is not going
// to without intervention.
//
// Being in a stalled state is not by itself a verdict — a torrent can stall
// briefly and recover. It is the caller's job to apply the time bound
// (ZeroStallAfter); this only answers "is it stalled right now".
//
// stoppeddl is included, and that is a deliberate choice rather than an
// oversight. This project only ever adds torrents running, never stopped, so a
// stopped download is a state nobody asked for — the user paused it by hand,
// or the client stopped it on a policy. Waiting the full two-day download
// deadline for something that is not moving would end in the same deletion,
// just later, and in the meantime the request is not looking for another
// release. Treating it as stalled instead lets the ordinary stall path handle
// it: abandon, blacklist, search again.
//
// Both spellings are listed because qBittorrent 4.x called the state pausedDL.
func (t TorrentStatus) Stalled() bool {
	switch t.State {
	case "stalleddl", "metadl", "forcedmetadl":
		return true
	case "stoppeddl", "pauseddl":
		return true
	}
	return false
}

// Downloaded reports whether the payload is complete and present, which is
// what the Plex refresh and the user notification depend on. A torrent that
// finished but whose files are gone is not a success.
func (t TorrentStatus) Downloaded() bool {
	return t.Done() && !t.Failed()
}
