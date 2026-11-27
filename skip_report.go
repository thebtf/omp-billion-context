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
	byFormat map[protocol]map[skipReason]uint64
	notified map[skipReason]time.Time
}

func (r *skipReport) record(reason skipReason, format protocol, now time.Time) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counts == nil {
		r.counts = make(map[skipReason]uint64, len(silentReasons))
		r.byFormat = make(map[protocol]map[skipReason]uint64, len(paths))
		r.notified = make(map[skipReason]time.Time, len(silentReasons))
	}
	r.counts[reason]++
	if r.byFormat[format] == nil {
		r.byFormat[format] = make(map[skipReason]uint64, len(silentReasons))
	}
	r.byFormat[format][reason]++
	if last, seen := r.notified[reason]; seen && now.Sub(last) < skipNoticeInterval {
		return r.counts[reason], false
	}
	r.notified[reason] = now
	return r.counts[reason], true
}

// detail reports the totals, the actionable total, and a per-protocol breakdown.
// The breakdown is what tells an operator which endpoint is losing compression:
// a rising missing_session_id on claude is a different problem from the same
// count on openai.
func (r *skipReport) detail() (map[string]uint64, uint64, map[string]map[string]uint64) {
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
	formats := make(map[string]map[string]uint64, len(r.byFormat))
	for format, reasons := range r.byFormat {
		row := make(map[string]uint64, len(reasons))
		for reason, count := range reasons {
			row[string(reason)] = count
		}
		formats[string(format)] = row
	}
	return counts, total, formats
}

// reportSkip records every bypass and logs only the ones an operator can act on.
// Ordinary traffic that CPA routes to plugin model routers is counted silently.
func (p *plugin) reportSkip(reason skipReason, format protocol, model string) {
	count, announce := p.skips.record(reason, format, time.Now())
	if !announce || !silentReasons[reason] {
		return
	}
	fields := map[string]any{
		"reason": string(reason), "format": string(format), "model": model, "occurrences": count,
	}
	p.log("warn", "billion-context kept a request uncompressed: "+describeSkip(reason), fields)
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
