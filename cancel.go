package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
)

// withStop makes the running sync, revert or reset cancellable from the
// status page. Call it with p.mu held, right after setting p.running.
func (p *plugin) withStop(ctx context.Context) context.Context {
	ctx, stop := context.WithCancel(ctx)
	p.stop = stop
	p.cancelled = false
	return ctx
}

// endStop releases the run's cancel function and reports whether the run was
// cancelled from the status page.
func (p *plugin) endStop() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stop != nil {
		p.stop()
		p.stop = nil
	}
	c := p.cancelled
	p.cancelled = false
	return c
}

// cancel stops whatever is running. A webhook run waiting behind it is
// dropped too, so cancelling doesn't just start another sync. With nothing
// running, it calls off a sync the webhook has scheduled.
func (p *plugin) cancel() string {
	p.mu.Lock()
	if p.running && p.stop != nil {
		p.cancelled = true
		p.rerun = false
		p.stop()
		p.mu.Unlock()
		return "running"
	}
	if p.running || p.timer == nil {
		p.mu.Unlock()
		return ""
	}
	p.timer.Stop()
	p.timer = nil
	p.due = time.Time{}
	p.mu.Unlock()
	if host := sdkruntime.Host(); host != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		meta, _ := readMeta(ctx, host)
		meta.PendingDue = ""
		_ = writeMeta(ctx, host, meta)
	}
	return "scheduled"
}

// markCancelled records a cancelled run in its summary. The cancellation
// error itself isn't an error worth showing.
func markCancelled(summary *runSummary, err error) error {
	summary.Cancelled = true
	if summary.Note == "" {
		summary.Note = "Cancelled from the status page. Work finished before that was kept; the next run picks up the rest."
	}
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// isCancellation reports whether an error being logged is just the run being
// cancelled.
func isCancellation(args []any) bool {
	for _, a := range args {
		if err, ok := a.(error); ok && errors.Is(err, context.Canceled) {
			return true
		}
	}
	return false
}

// cancelForm handles the progress card's Cancel button.
func (r *routesServer) cancelForm(req *pluginv1.HandleHTTPRequest) *pluginv1.HandleHTTPResponse {
	form, err := url.ParseQuery(string(req.GetBody()))
	if err != nil {
		return htmlMessage(http.StatusBadRequest, "Could not read the form.")
	}
	if subtle.ConstantTimeCompare([]byte(form.Get("token")), []byte(r.p.token())) != 1 {
		return htmlMessage(http.StatusForbidden, "This form has expired. Reload the status page and try again.")
	}
	switch r.p.cancel() {
	case "running":
		r.p.log.Info("run cancelled from the status page")
		return htmlMessage(http.StatusAccepted, "Cancelling. It stops within a few seconds, after saving what it has done.")
	case "scheduled":
		r.p.log.Info("scheduled sync called off from the status page")
		return htmlMessage(http.StatusOK, "The scheduled sync is called off. The next webhook schedules a new one.")
	}
	return htmlMessage(http.StatusOK, "Nothing is running or scheduled.")
}
