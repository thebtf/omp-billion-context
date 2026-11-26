import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { copyFile, mkdir, mkdtemp, readFile, readdir, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { basename, join, resolve } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import { Script } from 'node:vm';

const args = process.argv.slice(2);
assert.equal(args.length, 2, 'Usage: node test/smoke.mjs <cpa-executable> <plugin-library>');
const [cpaPath, pluginPath] = args.map(path => resolve(path));
const tmp = resolve(process.env.BILI_TEST_TMP ?? '.work/tmp');
await mkdir(tmp, { recursive: true });
const scratch = await mkdtemp(join(tmp, 'integration-'));
const children = [];
const requests = [];
const clientKey = 'synthetic-cpa-client';
const managementKey = 'synthetic-management';
const upstreamKey = 'synthetic-cpa-provider';
const model = 'existing-cpa-model';
const target = 'gpt-5.4';
const foldMarker = 'SHOULD_BE_FOLDED_';
const foldSummary = 'The earlier test turns discussed workspace setup, model routing and synthetic results. The remaining turns verify that the new context keeps its recent messages.';
let requestedFold = false;
let streamTail;

const upstream = createServer(async (req, res) => {
	try {
		const chunks = [];
		for await (const chunk of req) chunks.push(chunk);
		const body = JSON.parse(Buffer.concat(chunks));
		requests.push({ path: req.url, auth: req.headers.authorization, engineToken: req.headers['x-omp-bili-access'], body });
		const base = { id: 'chatcmpl-smoke', created: 1, model: body.model };
		if (body.stream) {
			res.writeHead(200, { 'content-type': 'text/event-stream' });
			if (!requestedFold && JSON.stringify(body.messages).includes(foldMarker)) {
				requestedFold = true;
				const argumentsJSON = JSON.stringify({ content: [{ startId: 'm00001', endId: 'm00004', summary: foldSummary }] });
				const tool = { index: 0, id: 'call_smoke', type: 'function', function: { name: 'compress', arguments: argumentsJSON } };
				const response = { ...base, object: 'chat.completion.chunk', choices: [{ index: 0, delta: { role: 'assistant', tool_calls: [tool] }, finish_reason: null }] };
				const completed = { ...base, object: 'chat.completion.chunk', choices: [{ index: 0, delta: {}, finish_reason: 'tool_calls' }] };
				return res.end(`data: ${JSON.stringify(response)}\n\ndata: ${JSON.stringify(completed)}\n\ndata: [DONE]\n\n`);
			}
			res.write(`data: ${JSON.stringify({ ...base, object: 'chat.completion.chunk', choices: [{ index: 0, delta: { role: 'assistant', content: 'smoke-ok' }, finish_reason: null }] })}\n\n`);
			if (streamTail) await streamTail;
			res.end(`data: ${JSON.stringify({ ...base, object: 'chat.completion.chunk', choices: [{ index: 0, delta: {}, finish_reason: 'stop' }] })}\n\ndata: [DONE]\n\n`);
		} else {
			res.writeHead(200, { 'content-type': 'application/json' });
			res.end(JSON.stringify({ ...base, object: 'chat.completion', choices: [{ index: 0, message: { role: 'assistant', content: 'smoke-ok' }, finish_reason: 'stop' }], usage: { prompt_tokens: 5, completion_tokens: 1, total_tokens: 6 } }));
		}
	} catch {
		res.writeHead(500).end();
	}
});

async function listen(server) {
	server.listen(0, '127.0.0.1');
	await once(server, 'listening');
	return server.address().port;
}

async function freePort() {
	const socket = createServer();
	const port = await listen(socket);
	await new Promise((done, fail) => socket.close(error => error ? fail(error) : done()));
	return port;
}

function start(executable, args, vars) {
	const baseline = Object.fromEntries(Object.entries(process.env).filter(([key]) => !/^(ACP_|BILI_|OMP_BILI_|NODE_OPTIONS$|PATH$|HTTP_PROXY$|HTTPS_PROXY$|ALL_PROXY$)/i.test(key)));
	const env = { ...baseline, PATH: '', TEMP: scratch, TMP: scratch, TMPDIR: scratch, HOME: scratch, USERPROFILE: scratch, APPDATA: scratch, LOCALAPPDATA: scratch, XDG_CONFIG_HOME: scratch, XDG_DATA_HOME: scratch, XDG_STATE_HOME: scratch, ...vars };
	const child = spawn(executable, args, { cwd: scratch, env, windowsHide: true, stdio: ['ignore', 'pipe', 'pipe'] });
	const record = { child, log: '', stopped: new Promise(done => child.once('close', done)) };
	child.on('error', error => { record.log += `${error}\n`; });
	for (const stream of [child.stdout, child.stderr]) stream.on('data', chunk => { record.log = (record.log + chunk).slice(-12000); });
	children.push(record);
	return record;
}

async function ready(url, record, headers) {
	for (let i = 0; i < 120; i++) {
		if (record.child.exitCode !== null) break;
		try {
			const response = await fetch(url, { headers, signal: AbortSignal.timeout(500) });
			if (response.ok) return response.json();
		} catch { }
		await delay(250);
	}
	throw new Error(`Startup failed at ${url}:\n${record.log}`);
}

try {
	const upstreamUrl = `http://127.0.0.1:${await listen(upstream)}`;
	const pluginDir = join(scratch, 'plugins');
	const cpaPort = await freePort();
	const cpaUrl = `http://127.0.0.1:${cpaPort}`;
	const cpaConfig = join(scratch, 'cpa.yaml');
	await mkdir(pluginDir);
	await copyFile(pluginPath, join(pluginDir, basename(pluginPath)));
	await writeFile(cpaConfig, `host: 127.0.0.1\nport: ${cpaPort}\nauth-dir: ${JSON.stringify(join(scratch, 'auth'))}\napi-keys: [${clientKey}, synthetic-other-client]\nrouting:\n  strategy: fill-first\nremote-management:\n  secret-key: ${managementKey}\n  disable-control-panel: true\nplugins:\n  enabled: true\n  dir: ${JSON.stringify(pluginDir)}\n  configs:\n    omp-billion-context:\n      enabled: true\nopenai-compatibility:\n  - name: fake\n    base-url: ${upstreamUrl}/v1\n    api-key-entries:\n      - api-key: ${upstreamKey}\n      - api-key: synthetic-cpa-backup\n    models:\n      - name: ${target}\n        alias: ${model}\n`);
	const cpa = start(cpaPath, ['-config', cpaConfig, '-local-model'], {});
	const managementHeaders = { 'x-management-key': managementKey };
	const listing = await ready(`${cpaUrl}/v0/management/plugins`, cpa, managementHeaders);
	const entry = listing.plugins.find(plugin => plugin.id === 'omp-billion-context');
	assert.equal(entry?.registered, true, JSON.stringify(listing));
	assert.equal(entry.effective_enabled, true);
	const menu = entry.menus.find(item => item.menu === 'Billion Context');
	assert.equal(menu?.path, '/v0/resource/plugins/omp-billion-context/webui');
	const shell = await fetch(cpaUrl + menu.path, { signal: AbortSignal.timeout(3000) });
	assert.equal(shell.status, 200);
	assert.match(shell.headers.get('content-type'), /^text\/html\b/);
	const shellHTML = await shell.text();
	assert.match(shellHTML, /CPA management key/);
	const script = shellHTML.match(/<script>([\s\S]+?)<\/script>/);
	assert(script, 'The shipped WebUI shell must include its login script');
	new Script(script[1], { filename: 'bili-webui.html' });
	assert(!shellHTML.includes(managementKey));
	assert(!shellHTML.includes('X-Omp-Bili-Access'));

	const bridge = (targets, init) => fetch(`${cpaUrl}/v0/management/plugins/omp-billion-context/webui-data?${targets}`, {
		signal: AbortSignal.timeout(35000), redirect: 'error', ...init,
	});
	const bridgeTarget = target => new URLSearchParams({ target });
	const readWebUI = target => bridge(bridgeTarget(target), { headers: managementHeaders });
	for (const headers of [{}, { authorization: `Bearer ${clientKey}` }]) {
		const rejected = await bridge(bridgeTarget('/__bili/overview'), { headers });
		assert.equal(rejected.status, 401);
		await rejected.text();
	}
	for (const target of ['https://outside.invalid/__bili/overview', '//outside.invalid/__bili/overview', '/v1/models',
		'/__bili/overview/extra', '/__bili/../__bili/overview', '/__bili/%6fverview', '/__bili/sessions/a%2Fb/detail',
		'/__bili/sessions/a\\b/detail', '/__bili/config', '/__bili/pricing', '/__bili/resign', '/__bili/upstream', '/__bili/logs', '/__bili/packs',
		'/__bili/packs/templates', '/__bili/packs/user/custom', '/__bili/update/state', '/__bili/update/install', '/__bili/changelog']) {
		const rejected = await readWebUI(target);
		assert.equal(rejected.status, 400, target);
		await rejected.text();
	}
	for (const targets of [new URLSearchParams(), new URLSearchParams([['target', '/__bili/overview'], ['target', '/__bili/sessions']])]) {
		const rejected = await bridge(targets, { headers: managementHeaders });
		assert.equal(rejected.status, 400);
		await rejected.text();
	}
	const write = await bridge(bridgeTarget('/__bili/config'), { method: 'POST', headers: managementHeaders });
	assert([404, 405].includes(write.status), `Management bridge accepted or misrouted POST (HTTP ${write.status})`);
	await write.text();
	const engineDirs = async () => (await readdir(scratch)).filter(name => name.startsWith('omp-billion-context-'));
	assert.deepEqual(await engineDirs(), [], 'Reading the shell and making denied requests must not start bili');

	const embed = await readWebUI('/__bili/?embed=0');
	assert.equal(embed.status, 200);
	assert.match(embed.headers.get('content-type'), /^text\/html\b/);
	const nativeHTML = await embed.text();
	assert.match(nativeHTML, /<body class="embed">/);
	assert.match(nativeHTML, /class="ver">v0\.1\.216/);
	assert.match(nativeHTML, /id="st-sessions"/);
	assert.match(nativeHTML, /data-nav="sessions"/);
	assert(!nativeHTML.includes(managementKey));
	assert(!nativeHTML.includes('X-Omp-Bili-Access'));
	const initialOverview = await readWebUI('/__bili/overview');
	assert.equal(initialOverview.status, 200, await initialOverview.clone().text());
	assert.equal((await initialOverview.json()).overview.requests, 0);
	console.log('CPA discovered the Billion Context menu and opened the native bili overview before any model traffic');

	const messages = [{ role: 'user', content: 'Synthetic conversation' }];
	function call(path, body, headers = {}) {
		return fetch(`${cpaUrl}/v1/${path}`, {
			method: 'POST', body: JSON.stringify(body), signal: AbortSignal.timeout(35000),
			headers: { 'content-type': 'application/json', authorization: `Bearer ${clientKey}`, 'x-acp-session': 'smoke-session', ...headers },
		});
	}
	const unauthorized = await fetch(`${cpaUrl}/v1/chat/completions`, { method: 'POST', headers: { authorization: 'Bearer invalid-fixture-key' } });
	assert.equal(unauthorized.status, 401);
	await unauthorized.text();
	assert.equal(requests.length, 0, 'Rejected traffic must not reach the provider');

	const bypass = await call('chat/completions', { model, messages }, { 'x-acp-session': '' });
	const bypassBody = await bypass.text();
	assert.equal(bypass.status, 200, bypassBody);
	assert.equal(requests.at(-1).body.model, target);
	assert.equal(requests.at(-1).body.tools?.length ?? 0, 0);
	const account = requests.at(-1).auth;
	assert([upstreamKey, 'synthetic-cpa-backup'].some(key => account === `Bearer ${key}`));

	for (const [path, input] of [['chat/completions', { messages }], ['responses', { input: messages }]]) {
		for (const stream of [false, true]) {
			let release;
			if (stream) streamTail = new Promise(done => { release = done; });
			try {
				const result = await call(path, { model, ...input, stream });
				assert.equal(result.status, 200);
				if (stream) {
					assert.match(result.headers.get('content-type'), /text\/event-stream/);
					const reader = result.body.getReader();
					const decoder = new TextDecoder();
					let prefix = '';
					while (!prefix.includes('smoke-ok')) {
						const chunk = await reader.read();
						assert(!chunk.done, 'Provider is still waiting to finish. The first text must already arrive.');
						prefix += decoder.decode(chunk.value, { stream: true });
					}
					release();
					while (!(await reader.read()).done) { }
				} else {
					assert.match(await result.text(), /smoke-ok/);
				}
			} finally {
				release?.();
				streamTail = undefined;
			}
			const received = requests.at(-1);
			assert.equal(received.path, '/v1/chat/completions');
			assert.equal(received.body.model, target);
			assert.equal(received.auth, account);
			console.log(`${path}, ${stream ? 'streaming' : 'JSON'}: CPA selected the configured provider model and account`);
		}
	}
	assert(requests.some(({ body }) => body.tools?.some(tool => ['acp_status', 'compress', 'acp_compress'].includes(tool.function?.name ?? tool.name))), 'The real billion-context process should inject context management tools');
	const opaque = await call('responses', { model, input: [{ type: 'reasoning', encrypted_content: 'opaque-fixture', summary: [] }, ...messages] });
	const opaqueBody = await opaque.text();
	assert.equal(opaque.status, 200, opaqueBody);
	assert.equal(requests.at(-1).body.tools?.length ?? 0, 0, 'Encrypted continuation must stay on the ordinary CPA route');
	for (const query of ['alt=sse', '$alt=sse']) {
		const sse = await call(`chat/completions?${query}`, { model, messages, stream: true });
		const sseBody = await sse.text();
		assert.equal(sse.status, 200, sseBody);
		assert.match(sse.headers.get('content-type'), /text\/event-stream/);
		assert.match(sseBody, /smoke-ok/);
		assert(requests.at(-1).body.tools?.length, 'CPA normalizes explicit SSE to the standard compression path');
	}
	const alternate = await call('chat/completions?alt=json', { model, messages });
	const alternateBody = await alternate.text();
	assert.equal(alternate.status, 200, alternateBody);
	assert.equal(requests.at(-1).body.tools?.length ?? 0, 0, 'Alternate modes must stay on the ordinary CPA route');
	console.log('Bundled bili injected tools; unsupported requests bypassed it');

	const overview = await readWebUI('/__bili/overview');
	assert.equal(overview.status, 200);
	const { version, overview: counters } = await overview.json();
	assert.equal(version, '0.1.216');
	assert(counters.live > 0 && counters.requests > 0, JSON.stringify(counters));
	const sessions = await readWebUI('/__bili/sessions?page=1&pageSize=20');
	assert.equal(sessions.status, 200);
	const sessionPage = await sessions.json();
	const session = sessionPage.sessions.find(item => item.live);
	assert(session?.id, 'The native Sessions list must include a live session from the model requests');
	const detail = await readWebUI(`/__bili/sessions/${encodeURIComponent(session.id)}/detail`);
	assert.equal(detail.status, 200);
	assert.equal((await detail.json()).id, session.id);
	console.log('Native Overview and Sessions reflected CPA model requests');

	const engines = await engineDirs();
	assert.equal(engines.length, 1, 'CPA must start exactly one bundled engine');
	const engineConfig = JSON.parse(await readFile(join(scratch, engines[0], 'bili.json'), 'utf8'));
	const unauthorizedEngine = await fetch(`http://127.0.0.1:${engineConfig.port}/__bili/stats`);
	assert.equal(unauthorizedEngine.status, 403);
	await unauthorizedEngine.text();
	assert(requests.every(request => request.engineToken === undefined), 'The engine access token must not reach the backend');

	const history = Array.from({ length: 12 }, (_, index) => ({
		role: index % 2 ? 'assistant' : 'user',
		content: `Turn ${index}: ${(index < 4 ? `${foldMarker}${index} ` : 'Recent synthetic context ').repeat(150)}`,
	}));
	const beforeFold = requests.length;
	const folded = await call('chat/completions', { model, stream: true, messages: history, prompt_cache_key: 'existing-cache-key' }, { 'x-acp-session': 'smoke-compress' });
	const foldedText = await folded.text();
	assert.equal(folded.status, 200, foldedText);
	assert.match(foldedText, /smoke-ok/);
	assert(!foldedText.includes('call_smoke'), 'The client must not execute the compression tool');
	assert(requestedFold);
	const replay = requests.slice(beforeFold + 1).find(({ body }) => JSON.stringify(body.messages).includes(foldSummary));
	assert(replay, 'billion-context must replay the request with the model-generated summary');
	const replayInput = JSON.stringify(replay.body.messages);
	for (let index = 1; index < 4; index++) {
		assert(!replayInput.includes(`${foldMarker}${index} `.repeat(10)), `The replay must omit the full text of folded turn ${index}`);
	}
	assert(replayInput.includes('Turn 11:'), 'The replay must preserve the recent conversation');
	const cacheFields = body => ({ key: body.prompt_cache_key, model: body.model });
	const anchor = replay.body.messages.findIndex(item => JSON.stringify(item).includes(foldSummary));
	assert(anchor >= 0, 'Fold replay has a summary anchor');
	assert.equal(JSON.stringify(replay.body.messages.slice(0, anchor)), JSON.stringify(requests[beforeFold].body.messages.slice(0, anchor)), 'The fold must preserve the provider prefix before its anchor');
	assert.equal(replay.body.prompt_cache_key, 'existing-cache-key');
	assert.deepEqual(cacheFields(replay.body), cacheFields(requests[beforeFold].body));
	assert.equal(replay.auth, account, 'The replay stays on the same available fill-first account');

	const followup = [...history, { role: 'assistant', content: 'smoke-ok' }, { role: 'user', content: 'Next turn after folding' }];
	const cycleHeaders = { 'x-acp-session': 'smoke-compress' };
	const next = await call('chat/completions', { model, stream: true, messages: followup, prompt_cache_key: 'existing-cache-key' }, cycleHeaders);
	const nextText = await next.text();
	assert.equal(next.status, 200, nextText);
	const nextBody = requests.at(-1).body;
	assert.equal(JSON.stringify(nextBody.messages.slice(0, anchor + 1)), JSON.stringify(replay.body.messages.slice(0, anchor + 1)), 'The next request keeps the provider prefix through the summary');
	assert.deepEqual(cacheFields(nextBody), cacheFields(replay.body));
	assert.equal(requests.at(-1).auth, account);

	const other = await call('chat/completions', { model, stream: true, messages: followup, prompt_cache_key: 'existing-cache-key' }, { ...cycleHeaders, authorization: ['Bearer', 'synthetic-other-client'].join(' ') });
	const otherText = await other.text();
	assert.equal(other.status, 200, otherText);
	assert(!JSON.stringify(requests.at(-1).body.messages).includes(foldSummary), 'A second CPA client must not inherit the first client’s summary');
	console.log('Bundled bili folded history; CPA preserved the account, cache key and prefix; caller state stayed isolated');
} catch (error) {
	for (const { log } of children) if (log) console.error(log);
	throw error;
} finally {
	for (const { child } of children) child.kill();
	await Promise.all(children.map(({ stopped }) => stopped));
	upstream.closeAllConnections();
	if (upstream.listening) await new Promise(done => upstream.close(done));
	await rm(scratch, { recursive: true, force: true, maxRetries: 4 });
}
