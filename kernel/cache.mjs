const valid = n => Number.isSafeInteger(n) && n >= 0;
const empty = () => ({ requests: 0, reported: 0, reads: 0, writes: 0, written: 0, pairedInput: 0, pairedRead: 0, pairs: 0 });

function add(total, sample) {
	total.requests++;
	if (valid(sample.readTokens)) { total.reported++; total.reads += sample.readTokens; }
	if (valid(sample.writeTokens)) { total.writes++; total.written += sample.writeTokens; }
	if (valid(sample.readTokens) && valid(sample.inputTokens) && sample.readTokens <= sample.inputTokens) {
		total.pairs++; total.pairedRead += sample.readTokens; total.pairedInput += sample.inputTokens;
	}
}

export function observeCache(record, phase, sample) {
	const stats = record.cacheStats ??= { total: empty(), before: empty(), after: empty() };
	add(stats.total, sample);
	if (phase === 'after_fold') add(stats.after, sample);
	if (phase !== 'auxiliary') stats.last = sample;
}

export function markFold(record) {
	if (record.cacheStats?.last) {
		add(record.cacheStats.before, record.cacheStats.last);
		record.cacheStats.last = null;
	}
}

function display(s) {
	return { requests: s.requests, reported: s.reported, readTokens: s.reported ? s.reads : null, writeTokens: s.writes ? s.written : null, inputTokens: s.pairs ? s.pairedInput : null, ratio: s.pairedInput ? s.pairedRead / s.pairedInput : null };
}

export function cacheView(records) {
	const merged = { total: empty(), before: empty(), after: empty() };
	for (const s of records) {
		if (!s) continue;
		for (const part of Object.keys(merged)) {
			for (const key of Object.keys(merged[part])) merged[part][key] += s[part]?.[key] ?? 0;
		}
	}
	return { ...display(merged.total), beforeFold: merged.before.requests ? display(merged.before) : null, afterFold: merged.after.requests ? display(merged.after) : null };
}
