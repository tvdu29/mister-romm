package app

import (
	"context"

	tea "charm.land/bubbletea/v2"
)

// A job runs long work (sync, downloads, tests) off the UI thread and reports back with messages.
// The UI keeps one waiting command per job; every message re-arms it until the job closes.
type job struct {
	ch     chan tea.Msg
	cancel context.CancelFunc
}

type fromJob struct {
	j   *job
	msg tea.Msg
}

type jobClosed struct{}

func startJob(run func(ctx context.Context, send func(tea.Msg))) (*job, tea.Cmd) {
	ctx, cancel := context.WithCancel(context.Background())
	j := &job{ch: make(chan tea.Msg, 256), cancel: cancel}
	go func() {
		defer close(j.ch)
		run(ctx, func(msg tea.Msg) {
			// deliver first: once the job is cancelled both cases of the select below can be
			// ready, and Go would pick one at random, losing the final message of a cancelled sync
			// or download (and leaving "Cancelling..." on screen)
			select {
			case j.ch <- msg:
				return
			default:
			}
			select {
			case j.ch <- msg:
			case <-ctx.Done():
			}
		})
	}()
	return j, j.wait()
}

func (j *job) wait() tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-j.ch
		if !ok {
			return fromJob{j: j, msg: jobClosed{}}
		}
		return fromJob{j: j, msg: msg}
	}
}
