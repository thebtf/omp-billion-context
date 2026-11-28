import assert from 'node:assert/strict';
import { test } from 'node:test';
import { mkdir, mkdtemp, rm, readFile, writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { createHost } from '../kernel/host.mjs';

const root = resolve(process.env.BILI_TEST_TMP ?? '.work/tmp');
const session = '1'.repeat(64), other = '2'.repeat(64);
const tail = 'Keep the last user instruction exactly.';
const summary = 'The earlier test messages settled the durable file format and the model callback contract.';
const history = Array.from({ length: 32 }, (_, i) => ({ role: i % 2 ? 'assistant' : 'user', content: i === 31 ? tail : `Turn ${i} ` + (`Evidence_${i} `).repeat(310) }));
const makeBody = format => format === 'openai-response' ? { model: 'test', input: history, instructions: 'Retain me', prompt_cache_key: 'explicit' } : { model: 'test', messages: history, ...(format === 'claude' ? { system: 'Retain me' } : { prompt_cache_key: 'explicit' }) };

for (const format of ['openai', 'claude', 'openai-response']) test(`${format}: fold, prefix, scoped recovery and reported cache`, async t => {
	await mkdir(root, { recursive: true }); const dir = await mkdtemp(join(root, 'kernel-check-')); t.after(() => rm(dir, { recursive: true, force: true }));
	let dispatch = createHost(dir); const body = makeBody(format); const req = { session, format, body };
	const prepare = (params = req) => dispatch({ ...params, op: 'prepare' });
	const first = await prepare(); assert(first.body.tools.length > 0); assert(JSON.stringify(first.body).includes('Evidence_2 '.repeat(20)));
	assert.equal((await dispatch({ op: 'dashboard', target: '/__bili/overview' })).cache.ratio, null);
	const rejected = await dispatch({ ...req, op: 'tool', name: 'compress', args: { content: 'not a ref' } }); assert.equal(rejected.ok, false); assert(rejected.feedback.length < 350);
	const long = await dispatch({ ...req, op: 'tool', name: 'compress', args: { content: [{ startId: 'm00002', endId: 'm00009', summary: 'x'.repeat(21150) }] } }); assert.equal(long.ok, false); assert.match(long.feedback, /21150.*20000/);
	await dispatch({ op: 'cache', session, phase: 'turn', usage: { inputTokens: 100, readTokens: 80 } });
	await dispatch({ op: 'cache', session, phase: 'auxiliary', usage: { inputTokens: 200, readTokens: 0 } });
	await dispatch({ op: 'configure', visibilityMarkers: false });
	const accepted = await dispatch({ ...req, op: 'tool', name: 'compress', args: { content: JSON.stringify({ startId: 'm00002', endId: 'm00009', summary }) } });
	assert.equal(accepted.ok, true, accepted.feedback); assert.equal(accepted.marker, undefined);
	const next = (await prepare()).body; const nextWire = JSON.stringify(next); assert(nextWire.includes(summary));
	const anchor = (next.input ?? next.messages).find(m => JSON.stringify(m).includes(summary)); assert.equal(anchor.role, 'user', 'A model-written summary cannot become a system or developer instruction'); assert(!nextWire.includes('Evidence_2 '.repeat(20))); assert(nextWire.includes(tail)); assert.equal(next.prompt_cache_key, body.prompt_cache_key);
	await dispatch({ op: 'cache', session, phase: 'after_fold', usage: { inputTokens: 50, readTokens: 0 } });
	await dispatch({ op: 'cache', session, phase: 'turn', usage: {} });
	dispatch = createHost(dir); const restored = (await prepare()).body; assert.deepEqual(restored, next); assert.equal((await dispatch({ op: 'dashboard', target: '/__bili/settings' })).visibilityMarkers, false);
	const view = await dispatch({ op: 'dashboard', target: `/__bili/sessions/${session}/detail` }); assert.equal(view.blocks[0].summary, summary); assert.equal(view.cache.ratio, 80 / 350); assert.equal(view.cache.beforeFold.ratio, .8); assert.equal(view.cache.afterFold.ratio, 0); assert.equal(view.cache.reported, 3); assert.equal(view.cache.requests, 4); assert.equal(view.cache.writeTokens, null);
	const untouched = (await prepare({ ...req, session: other })).body; assert(!JSON.stringify(untouched).includes(summary)); assert(JSON.stringify(untouched).includes('Evidence_2 '.repeat(20)));
});

test('legacy session appears in the archive but is never rewritten', async t => {
	await mkdir(root, { recursive: true }); const dir = await mkdtemp(join(root, 'kernel-archive-')); t.after(() => rm(dir, { recursive: true, force: true }));
	await mkdir(join(dir, 'anthropic')); const path = join(dir, 'anthropic', 'loopback_00112233445566778899aabb.json');
	const old = JSON.stringify({ version: 1, id: 'synthetic-old', savedAt: '2026-01-01T12:00:00Z', meta: { hostTitle: 'Original work', protocol: 'anthropic' }, state: { blocks: [{ blockId: 'b2', active: true, summary }] } }); await writeFile(path, old);
	const api = createHost(dir); const page = await api({ op: 'dashboard', target: '/__bili/sessions?q=Original' }); assert.equal(page.total, 1); assert(page.sessions[0].legacy); assert.equal((await api({ op: 'dashboard', target: `/__bili/sessions/${page.sessions[0].id}/detail` })).blocks[0].summary, summary); assert.equal(await readFile(path, 'utf8'), old);
});


test('a damaged unrelated session cannot block a healthy model turn', async t => {
	await mkdir(root, { recursive: true }); const dir = await mkdtemp(join(root, 'kernel-isolation-')); t.after(() => rm(dir, { recursive: true, force: true }));
	await mkdir(join(dir, 'kernel-v1')); const broken = join(dir, 'kernel-v1', other + '.session'); await writeFile(broken, 'incomplete legacy data');
	const host = createHost(dir); const result = await host({ op: 'prepare', session, format: 'openai', body: makeBody('openai') });
	assert(result.body.tools.some(item => item.function?.name === 'compress')); assert.equal(await readFile(broken, 'utf8'), 'incomplete legacy data');
});


test('overlapping observations cannot overwrite a confirmed fold for the same session', async t => {
	await mkdir(root, { recursive: true }); const dir = await mkdtemp(join(root, 'kernel-order-')); t.after(() => rm(dir, { recursive: true, force: true }));
	const host = createHost(dir); const base = { session, format: 'openai', body: makeBody('openai') }; await host({ ...base, op: 'prepare' });
	const fold = host({ ...base, op: 'tool', name: 'compress', args: { content: [{ startId: 'm00002', endId: 'm00009', summary }] } });
	await Promise.all([fold, ...Array.from({ length: 16 }, () => host({ op: 'cache', session, phase: 'turn', usage: { readTokens: 3, inputTokens: 8 } }))]); assert((await fold).ok);
	const detail = await createHost(dir)({ op: 'dashboard', target: `/__bili/sessions/${session}/detail` }); assert.equal(detail.blocks[0].summary, summary); assert.equal(detail.cache.reported, 16); assert.equal(detail.cache.readTokens, 48);
});

test('stored Responses recommends a pre-answer fold and preserves the caller’s tools, instructions and cache key', async t => {
	await mkdir(root, { recursive: true }); const dir = await mkdtemp(join(root, 'kernel-stored-')); t.after(() => rm(dir, { recursive: true, force: true }));
	const host = createHost(dir);
	const input = history.map((msg, i) => ({ ...msg, content: i === history.length - 1 ? tail : (`Evidence_${i} `).repeat(2600) }));
	const body = { ...makeBody('openai-response'), input, stream: true, store: true, tools: [{ type: 'function', name: 'lookup', parameters: { type: 'object', properties: {} } }] };
	const req = { session, format: 'openai-response', body, singleResponse: true };
	const first = await host({ ...req, op: 'prepare' }); assert.deepEqual(first.body, body); assert.equal(first.foldRecommended, true);
	const plan = await host({ ...req, op: 'overflow' }); assert(plan.segments.length > 0); const { startId, endId } = plan.segments[0];
	const fold = await host({ ...req, op: 'tool', name: 'compress', args: { content: [{ startId, endId, summary }] } }); assert(fold.ok, fold.feedback);
	const { body: final } = await host({ ...req, op: 'prepare' }); assert.deepEqual({ ...final, input }, body);
	assert(final.input.some(item => item.role === 'user' && JSON.stringify(item).includes(summary))); assert(JSON.stringify(final.input).includes('Evidence_0 '.repeat(10))); assert(!JSON.stringify(final.input).includes('Evidence_1 '.repeat(10))); assert(JSON.stringify(final.input).includes(tail));
});
