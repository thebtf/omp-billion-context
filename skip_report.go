package main

import (
	"sync"
	"time"
)

// skipNoticeInterval bounds how often one bypass reason reaches CPA's log, so a
// busy CPA does not turn a per-request diagnosis into a log flood.
const skipNoticeInterval = 10 * time.Minute

type skipReport struct {
	mu       sync.Mutex
	counts   map[skipReason]uint64
	notified map[skipReason]time.Time
}

func (r *skipReport) record(reason skipReason, now time.Time) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counts == nil {
		r.counts = make(map[skipReason]uint64, len(silentReasons))
		r.notified = make(map[skipReason]time.Time, len(silentReasons))
	}
	r.counts[reason]++
	if last, seen := r.notified[reason]; seen && now.Sub(last) < skipNoticeInterval {
		return r.counts[reason], false
	}
	r.notified[reason] = now
	return r.counts[reason], true
}

func (r *skipReport) snapshot() (map[string]uint64, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	counts := make(map[string]uint64, len(r.counts))
	var total uint64
	for reason, count := range r.counts {
		counts[string(reason)] = count
		if silentReasons[reason] {
			total += count
		}
	}
	return counts, total
}

// reportSkip records every bypass and logs only the ones an operator can act on.
// Ordinary traffic that CPA routes to plugin model routers is counted silently.
func (p *plugin) reportSkip(reason skipReason, model string) {
	count, announce := p.skips.record(reason, time.Now())
	if !announce || !silentReasons[reason] {
		return
	}
	p.log("warn", "billion-context kept a request uncompressed: "+describeSkip(reason), map[string]any{
		"reason": string(reason), "model": model, "occurrences": count,
	})
}

// log reports through CPA's own logger so the plugin never invents a second log
// destination. A missing host callback simply drops the diagnostic.
func (p *plugin) log(level, message string, fields map[string]any) {
	if p.call == nil {
		return
	}
	payload := map[string]any{"level": level, "message": message}
	if len(fields) > 0 {
		payload["fields"] = fields
	}
	_ = p.call("host.log", payload, nil)
}
