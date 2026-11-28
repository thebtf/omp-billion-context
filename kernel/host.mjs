import { markFold, observeCache } from './cache.mjs';
import { createCore, createInitialState, defaultConfig, defaultCountTokens, parseCompressArgs, COMPRESS_TOOL, COMPRESS_TOOL_OPENAI, COMPRESS_TOOL_RESPONSES, ACP_STATUS_TOOL, ACP_STATUS_TOOL_OPENAI, ACP_STATUS_TOOL_RESPONSES } from 'acp-kernel';
import { openaiToCore, coreToOpenai, responsesToCore, patchResponsesInput, anthropicToCore, coreToAnthropic } from 'acp-kernel/wire';
import { createStore } from './store.mjs';

const core = createCore();
const config = defaultConfig(200000);
const formats = {
	openai: { decode: openaiToCore, tools: [COMPRESS_TOOL_OPENAI, ACP_STATUS_TOOL_OPENAI] },
	'openai-response': { decode: responsesToCore, tools: [COMPRESS_TOOL_RESPONSES, ACP_STATUS_TOOL_RESPONSES] },
	claude: { decode: anthropicToCore, tools: [COMPRESS_TOOL, ACP_STATUS_TOOL] },
};
const nameOf = tool => tool.function?.name ?? tool.name;
const guidance = 'ACP context management: when older work can be summarized, call compress with message refs and a faithful summary. Keep decisions, constraints, and unresolved work. Use acp_status to inspect the current ranges. Continue the original task after compression.';

function encode(format, original, decoded, messages, feedback, tools) {
	const body = { ...original, tools: [...(original.tools ?? []), ...tools] };
	const reminder = feedback ? [{ role: 'user', content: 'Context tool result:\n' + feedback }] : [];
	if (format === 'openai') {
		const first = original.messages.findIndex(m => m.role !== 'system' && m.role !== 'developer');
		const prefix = first < 0 ? original.messages : original.messages.slice(0, first);
		body.messages = [...prefix, { role: 'system', content: guidance }, ...coreToOpenai(messages), ...reminder];
	} else if (format === 'claude') {
		const system = Array.isArray(original.system) ? original.system : original.system ? [{ type: 'text', text: original.system }] : [];
		body.system = [...system, { type: 'text', text: guidance }];
		body.messages = [...coreToAnthropic(messages, decoded.cacheControls), ...reminder];
	} else {
		body.instructions = [original.instructions, guidance].filter(Boolean).join('\n\n');
		const input = patchResponsesInput(decoded, messages);
		body.input = reminder.length ? [...(Array.isArray(input) ? input : [{ role: 'user', content: input }]), ...reminder] : input;
	}
	return body;
}

function displayTitle(messages) {
	return messages.filter(m => m.role === 'user' && m.contentType === 'text').map(m => m.text?.trim()).find(Boolean)?.slice(0, 400) ?? '';
}

export function createHost(directory) {
	const store = createStore(directory);
	async function perform(request) {
		if (request.op === 'dashboard') return store.dashboard(request.target);
		if (request.op === 'configure') return store.configure(request.visibilityMarkers);
		if (!/^[0-9a-f]{64}$/.test(request.session)) throw new Error('Invalid session ID');
		if (request.op === 'cache') {
			const record = await store.get(request.session);
			if (record) { observeCache(record, request.phase, request.usage ?? {}); await store.put(record); }
			return {};
		}
		const adapter = Object.hasOwn(formats, request.format) && formats[request.format];
		if (!adapter || !request.body || !['prepare', 'tool', 'overflow'].includes(request.op)) throw new Error('Unsupported kernel operation');
		const decoded = adapter.decode(request.body);
		const messages = decoded.msgs;
		const now = new Date().toISOString();
		const record = await store.get(request.session) ?? { id: request.session, title: displayTitle(messages), model: request.body.model, protocol: request.format, blocks: 0, tokensSaved: 0, blockList: [], events: [], state: createInitialState() };
		const tokens = messages.reduce((n, m) => n + defaultCountTokens(m.text ?? ''), 0);
		const tools = adapter.tools.filter(t => !(request.body.tools ?? []).some(existing => nameOf(existing) === nameOf(t)));
		if (request.op === 'overflow') {
			const covered = new Set(record.state.blocks.filter(b => b.active).flatMap(b => b.effectiveMessageIds));
			const segments = []; let chars = 0, group = null;
			const goal = messages.reduce((n, m) => n + (m.text?.length ?? 0), 0) * 0.6;
			for (const m of messages.slice(0, -config.preserveRecentMessages)) {
				if (chars >= goal || segments.length >= 8) break;
				const ref = record.state.messageRefs.byRaw[m.id];
				if (covered.has(m.id) || !ref || !m.text || m.contentType === 'reasoning' || m.role === 'system' || m.role === 'developer') { group = null; continue; }
				const line = `${ref} ${m.role}: ${m.text}\n`;
				if (line.length > 128000) { group = null; continue; }
				if (!group || group.content.length + line.length > 128000) { group = { startId: ref, endId: ref, content: '' }; segments.push(group); }
				group.endId = ref; group.content += line; chars += line.length;
			}
			return { segments: segments.filter(s => s.content.length >= config.compress.minCompressRange) };
		}
		if (request.op === 'prepare') {
			if (!tools.length) return { body: request.body };
			const turn = core.processTurn({ messages, state: record.state, config, tokenCount: tokens, renderTags: 'text-only' });
			record.state = turn.state;
			record.updatedAt = now;
			await store.put(record);
			// The kernel marks summaries as system for its internal pruning pipeline. On the wire they carry conversation data, not instructions.
			const summaryIds = new Set(turn.state.blocks.map(b => 'acp_summary_' + b.blockId));
			const projected = turn.messages.map(m => m.role === 'system' && summaryIds.has(m.id) ? { ...m, role: 'user' } : m);
			return { body: encode(request.format, request.body, decoded, projected, request.feedback, tools) };
		}
		if (!tools.some(t => nameOf(t) === request.name)) throw new Error('Tool belongs to the calling client');
		if (request.name === 'acp_status') {
			const report = core.status(record.state, tokens, config);
			return { ok: true, feedback: JSON.stringify(report).slice(0, 6000) };
		}
		async function reject(reason) {
			const feedback = reason.slice(0, 280);
			record.events = [...record.events.slice(-63), { kind: 'rejected', message: feedback, at: now }];
			await store.put(record); return { ok: false, feedback };
		}
		let parsed = parseCompressArgs(request.args);
		if (parsed.diagnostics.kind === 'content-not-array' && typeof request.args?.content === 'string' && request.args.content.trimStart().startsWith('{')) {
			let entry;
			try { entry = JSON.parse(request.args.content) } catch { /* Preserve the parser's rejection for truncated input. */ }
			if (entry && typeof entry.summary === 'string' && typeof (entry.startId ?? entry.startRef) === 'string' && typeof (entry.endId ?? entry.endRef) === 'string') {
				parsed = parseCompressArgs({ content: [entry] });
			}
		}
		const { ranges, diagnostics } = parsed;
		if (!diagnostics.ok || diagnostics.invalidItems || diagnostics.kind !== 'ok' || !ranges.length) return reject('Invalid range. Use content: [{startId:"m00001", endId:"m00006", summary:"..."}] with refs from this conversation.');
		for (const range of ranges) {
			if (range.summary.length > config.compress.maxSummaryLength) return reject(`Summary has ${range.summary.length} characters; limit ${config.compress.maxSummaryLength}. Shorten it or split the range.`);
		}
		const folded = core.applyCompression({ messages, state: structuredClone(record.state), config, ranges });
		if (folded.result.errors.length || !folded.result.blocksCreated) return reject(folded.result.errors[0] ?? 'No eligible messages in this range. Use acp_status for current references.');
		markFold(record);
		record.lastFoldAt = now;
		record.state = folded.state;
		const savedPerBlock = b => Math.max(0, b.compressedTokens - defaultCountTokens(b.summary));
		record.blockList = folded.state.blocks.filter(b => b.active).map(b => ({ id: b.blockId, summary: b.summary, tokensSaved: savedPerBlock(b) }));
		record.blocks = record.blockList.length;
		record.tokensSaved = record.blockList.reduce((n, b) => n + b.tokensSaved, 0);
		record.updatedAt = now;
		const feedback = `Compressed ${ranges.map(r => r.startRef + '–' + r.endRef).join(', ')} into ${folded.result.blocksCreated} block(s).`;
		record.events = [...record.events.slice(-63), { kind: 'compressed', message: feedback, at: now }];
		await store.put(record);
		const settings = await store.settings();
		return { ok: true, feedback, ...(settings.visibilityMarkers ? { marker: '[ACP] ' + feedback } : {}) };
	}
	const tails = new Map();
	return function dispatch(request) {
		const id = request.session;
		if (typeof id !== 'string' || !/^[a-f0-9]{64}$/.test(id)) return perform(request);
		// A caller can lose interest in an IPC result while its durable write is still in flight.
		const work = (tails.get(id) ?? Promise.resolve()).catch(() => { }).then(() => perform(request));
		tails.set(id, work);
		return work.finally(() => { if (tails.get(id) === work) tails.delete(id) });
	};
}
