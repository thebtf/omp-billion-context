import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { runInNewContext } from 'node:vm';
import { setImmediate as tick } from 'node:timers/promises';

const html = readFileSync(new URL('../webui.html', import.meta.url), 'utf8');
const source = html.match(/<script>([\s\S]+?)<\/script>/)[1];
const keyName = 'omp-billion-context.management-key';
const fixtureKey = 'synthetic-management';
const overview = { sessions: 0, blocks: 0, tokensSaved: 0, recent: [], visibilityMarkers: true };

function tab(fetch, persisted = new Map()) {
	const listeners = new Map(); let reloaded = 0;
	const storage = data => ({ getItem: k => data.get(k) ?? null, setItem: (k, v) => data.set(k, v), removeItem: k => data.delete(k) });
	const node = tag => ({
		tag, childNodes: [], attrs: {}, events: {}, dataset: {}, value: '',
		append(child) { this.childNodes.push(child) }, replaceChildren(...children) { this.childNodes = children },
		setAttribute(k, v) { this.attrs[k] = v }, removeAttribute(k) { delete this.attrs[k] },
		addEventListener(k, v) { this.events[k] = v }, focus() { document.activeElement = this }
	});
	const document = { body: node('body'), documentElement: node('html'), createElement: node, activeElement: null };
	const find = (fn, n = document.body) => { if (fn(n)) return n; for (const c of n.childNodes ?? []) { const found = find(fn, c); if (found) return found } };
	const item = id => find(n => n.id === id);
	const location = { hash: '', reload() { reloaded++ } };
	const window = { fetch, addEventListener: (k, fn) => listeners.set(k, fn) };
	const session = new Map(); runInNewContext(source, { document, window, location, history: { replaceState() { } }, localStorage: storage(persisted), sessionStorage: storage(session), navigator: { language: 'en' }, URLSearchParams, AbortSignal, Intl, console });
	return {
		persisted, session, item, get reloaded() { return reloaded }, storageEvent: (key, newValue) => listeners.get('storage')({ key, newValue }),
		login(value) { item('management-key').value = value; find(n => n.tag === 'form').events.submit({ preventDefault() { } }) },
		click(id) { item(id).events.click({ preventDefault() { } }) }
	};
}

const response = (status, result = overview) => ({ status, ok: status === 200, json: async () => result });
const auth = async (_url, init) => response(init.headers['X-Management-Key'] === fixtureKey ? 200 : 401);
const a = tab(auth); a.login('wrong'); await tick(); assert.equal(a.persisted.has(keyName), false); assert(a.item('management-key'));
a.login(fixtureKey); await tick(); assert.equal(a.persisted.get(keyName), fixtureKey); assert(a.item('cache-share'));
a.click('dismiss-notice'); assert.equal(a.item('priority-notice'), undefined);
const b = tab(auth, a.persisted); await tick(); assert(b.item('cache-share')); assert.equal(b.item('priority-notice'), undefined);
a.session.set(keyName, fixtureKey); a.click('lock'); assert(!a.persisted.has(keyName)); assert(!a.session.has(keyName));
b.storageEvent('unrelated', null); b.storageEvent(keyName, 'other'); assert.equal(b.reloaded, 0); b.storageEvent(keyName, null); assert.equal(b.reloaded, 1);
let finish;
const pending = tab(() => new Promise(resolve => { finish = resolve })); pending.login(fixtureKey); pending.storageEvent(keyName, null); finish(response(200)); await tick(); assert.equal(pending.persisted.has(keyName), false); assert.equal(pending.item('cache-share'), undefined);
console.log('Verified accepted and rejected credentials, durable dismissal, cross-tab lock and lock during pending login.');
