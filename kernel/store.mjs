import { cacheView } from './cache.mjs';
import { createHash, randomUUID } from 'node:crypto';
import { open, lstat, mkdir, readdir, rename, rm } from 'node:fs/promises';
import { join } from 'node:path';

const keyPattern = /^[a-f0-9]{64}$/;
const maximumBytes = 64 * 1024 * 1024;
const number = value => Number.isFinite(value) && value >= 0 ? value : null;
const text = value => typeof value === 'string' ? value : '';
const date = value => value != null && value !== '' && Number.isFinite(new Date(value).valueOf()) ? new Date(value).toISOString() : null;

async function directory(path, create) {
	if (create) await mkdir(path, { recursive: true, mode: 0o700 });
	const info = await lstat(path);
	if (info.isSymbolicLink() || !info.isDirectory()) throw new Error('Unsafe session directory');
}

async function readFile(path, headerOnly = false) {
	const stat = await lstat(path);
	if (!stat.isFile() || stat.isSymbolicLink() || stat.size > maximumBytes) throw new Error('Unsupported session file');
	const file = await open(path, 'r');
	try {
		if (!headerOnly) return await file.readFile('utf8');
		const buffer = Buffer.alloc(Math.min(8192, stat.size));
		const { bytesRead } = await file.read(buffer, 0, buffer.length, 0);
		const end = buffer.subarray(0, bytesRead).indexOf(10);
		if (end < 0) throw new Error('Incomplete session record');
		return buffer.toString('utf8', 0, end);
	} finally { await file.close(); }
}

async function save(path, data) {
	if (Buffer.byteLength(data) > maximumBytes) throw new Error('Session exceeds its storage limit');
	const pending = path + '.' + randomUUID() + '.tmp';
	const file = await open(pending, 'wx', 0o600);
	try {
		try { await file.writeFile(data); await file.sync(); } finally { await file.close(); }
		await rename(pending, path);
	} finally { await rm(pending, { force: true }); }
}

function row(record) {
	return { id: record.id, title: text(record.title).slice(0, 400), model: text(record.model).slice(0, 256), protocol: text(record.protocol).slice(0, 64), updatedAt: date(record.updatedAt), lastFoldAt: record.lastFoldAt ?? null, lastEvent: record.events?.at(-1)?.kind ?? null, blocks: number(record.blocks), tokensSaved: number(record.tokensSaved) };
}

function legacyView(source) {
	const record = source?.payload ?? source;
	if (!record || typeof record.id !== 'string' || !Array.isArray(record.state?.blocks)) return null;
	const meta = record.meta ?? {};
	const blocks = record.state.blocks.filter(block => block && block.active !== false).map(block => ({ id: text(block.blockId ?? block.id), summary: text(block.summary), tokensSaved: number(block.tokensCompressed) }));
	const id = 'legacy-' + createHash('sha256').update(record.id).digest('hex');
	const session = row({ id, title: meta.hostTitle || record.metadata?.title || meta.title || record.messages?.find(m => m?.role === 'user')?.text, model: meta.model, protocol: meta.protocol ?? record.protocol, updatedAt: record.savedAt ?? source.savedAt, blocks: blocks.length, tokensSaved: record.stats?.tokensSaved });
	return { session: { ...session, legacy: true }, blocks, events: [] };
}

export function createStore(root) {
	const dir = join(root, 'kernel-v1');
	const rows = new Map();
	const archives = new Map();
	const caches = new Map();
	let loaded;
	let indexed;
	let visibility = true;
	let configWrites = Promise.resolve();

	async function scanLegacy(parent, depth = 0) {
		for (const entry of await readdir(parent, { withFileTypes: true })) {
			const path = join(parent, entry.name);
			if (entry.isSymbolicLink() || path === dir) continue;
			if (depth < 2 && entry.isDirectory()) { await scanLegacy(path, depth + 1); continue; }
			if (!entry.isFile() || !entry.name.endsWith('.json') || entry.name.endsWith('.content-store.json')) continue;
			try {
				const view = legacyView(JSON.parse(await readFile(path)));
				if (view) { rows.set(view.session.id, view.session); archives.set(view.session.id, path); }
			} catch (error) { if (!['ENOENT'].includes(error.code) && !(error instanceof SyntaxError) && error.message !== 'Unsupported session file') throw error; }
		}
	}
	async function load() {
		await directory(root, true); await directory(dir, true);
		try {
			const config = JSON.parse(await readFile(join(dir, 'settings.json')));
			if (typeof config.visibilityMarkers !== 'boolean') throw new Error('Invalid notification settings');
			visibility = config.visibilityMarkers;
		} catch (error) { if (error.code !== 'ENOENT') throw error; }
	}
	async function buildIndex() {
		for (const entry of await readdir(dir, { withFileTypes: true })) {
			const id = entry.name.replace(/\.session$/, '');
			if (!entry.isFile() || entry.name !== id + '.session' || !keyPattern.test(id)) continue;
			const meta = JSON.parse(await readFile(join(dir, entry.name), true));
			if (meta.id !== id) throw new Error('Session identity mismatch');
			const { cacheStats, ...summary } = meta;
			if (rows.has(id)) continue;
			rows.set(id, summary);
			if (cacheStats) caches.set(id, cacheStats);
		}
		await scanLegacy(root);
	}
	function ready() { return loaded ??= load().catch(error => { loaded = undefined; throw error; }); }
	async function get(id) {
		if (!keyPattern.test(id)) throw new Error('Invalid session ID');
		await ready();
		let data;
		try { data = await readFile(join(dir, id + '.session')); } catch (error) { if (error.code === 'ENOENT') return null; throw error; }
		const record = JSON.parse(data.slice(data.indexOf('\n') + 1));
		if (record.id !== id) throw new Error('Session identity mismatch');
		return record;
	}
	async function put(record) {
		if (!keyPattern.test(record.id)) throw new Error('Invalid session ID');
		await ready();
		const meta = row(record);
		await save(join(dir, record.id + '.session'), JSON.stringify({ ...meta, cacheStats: record.cacheStats }) + '\n' + JSON.stringify(record));
		rows.set(record.id, meta);
		if (record.cacheStats) caches.set(record.id, record.cacheStats);
	}
	async function settings() { await ready(); return { visibilityMarkers: visibility }; }
	function configure(value) {
		if (typeof value !== 'boolean') throw new Error('Expected boolean');
		const pending = configWrites.then(async () => {
			await ready(); await save(join(dir, 'settings.json'), JSON.stringify({ visibilityMarkers: value }));
			visibility = value; return { visibilityMarkers: value };
		});
		configWrites = pending.catch(() => { }); return pending;
	}
	async function dashboard(target) {
		await ready();
		const url = new URL(target, 'http://local.invalid');
		if (url.pathname === '/__bili/settings') return settings();
		// Serving a model turn must never depend on scanning unrelated archives.
		await (indexed ??= buildIndex().catch(error => { indexed = undefined; throw error }));
		const detail = /^\/__bili\/sessions\/((?:legacy-)?[a-f0-9]{64})\/detail$/.exec(url.pathname);
		if (detail) {
			const id = detail[1]; if (!rows.has(id)) return null;
			if (archives.has(id)) return legacyView(JSON.parse(await readFile(archives.get(id))));
			const record = await get(id);
			return { session: row(record), cache: cacheView([record.cacheStats]), blocks: record.blockList ?? [], events: record.events ?? [] };
		}
		const list = [...rows.values()].sort((a, b) => (b.updatedAt ?? '').localeCompare(a.updatedAt ?? '') || a.id.localeCompare(b.id));
		if (url.pathname === '/__bili/overview') {
			const total = key => list.some(item => item[key] == null) ? null : list.reduce((n, item) => n + item[key], 0);
			const latest = list.filter(s => s.lastFoldAt && !s.legacy).sort((a, b) => b.lastFoldAt.localeCompare(a.lastFoldAt))[0];
			const recentFold = latest ? await get(latest.id) : null;
			const latestFold = recentFold ? { session: latest, block: recentFold.blockList.at(-1) ?? null, events: recentFold.events.slice(-3).reverse() } : null;
			return { latestFold, cache: cacheView(caches.values()), sessions: list.length, blocks: total('blocks'), tokensSaved: total('tokensSaved'), recent: list.slice(0, 12), visibilityMarkers: visibility };
		}
		if (url.pathname !== '/__bili/sessions') return null;
		const needle = (url.searchParams.get('q') ?? '').slice(0, 200).toLowerCase();
		const matches = list.filter(r => [r.title, r.model, r.protocol, r.id].some(v => v.toLowerCase().includes(needle)));
		const page = Math.max(1, Number.parseInt(url.searchParams.get('page'), 10) || 1);
		const size = Math.min(100, Math.max(1, Number.parseInt(url.searchParams.get('pageSize'), 10) || 25));
		return { sessions: matches.slice((page - 1) * size, page * size), total: matches.length };
	}
	return { get, put, dashboard, configure, settings };
}
