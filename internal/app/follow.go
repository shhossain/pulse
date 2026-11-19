package app

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"pulse/internal/follow"
	"pulse/internal/hubctl"
	"pulse/internal/model"
)

// FollowHub shows a followed hub's runs (pulse hub follow) next to this
// machine's, checking every 30 seconds whether that's set up or changed.
func (a *App) FollowHub(ctx context.Context) {
	var cur follow.Config
	var stop context.CancelFunc
	var gen atomic.Int64 // which follower may still update the runs shown
	warned := follow.Config{}
	check := func() {
		fc, ok := follow.Load()
		if ok && fc == cur && stop != nil {
			return
		}
		if stop != nil { // changed or removed: stop the old one
			stop()
			stop = nil
			gen.Add(1) // a late update from it is ignored
			a.Srv.SetHubRuns(nil)
		}
		cur = fc
		if !ok {
			return
		}
		secret := hubctl.LoadSecret(hubctl.Exec, hubctl.FollowService, fc.ClientID)
		if secret == "" {
			if warned != fc {
				warned = fc
				log.Printf("follow %s: no secret for %s in the Keychain (pulse hub unfollow, then follow)", fc.URL, fc.ClientID)
			}
			cur = follow.Config{} // look again next time
			return
		}
		fctx, cancel := context.WithCancel(ctx)
		stop = cancel
		mine := gen.Load()
		f := &follow.Follower{Config: fc, Secret: secret, OnRuns: func(runs []model.Run) {
			if gen.Load() == mine && fctx.Err() == nil {
				a.Srv.SetHubRuns(runs)
			}
		}}
		go f.Run(fctx)
		log.Printf("following %s", fc.URL)
	}
	check()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if stop != nil {
				stop()
			}
			return
		case <-t.C:
			check()
		}
	}
}
