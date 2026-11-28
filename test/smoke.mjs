import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { createHash } from 'node:crypto';
import { createReadStream } from 'node:fs';
import { copyFile, mkdir, mkdtemp, readFile, readdir, rename, rm, stat, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { basename, extname, join, resolve } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import { Script } from 'node:vm';

const args = process.argv.slice(2);
assert(args.length === 2 || args.length === 3, 'Usage: node test/smoke.mjs <cpa-executable> <plugin-library> [replacement-library]');
const [cpaPath, pluginPath] = args.slice(0, 2).map(path => resolve(path));
const replacementPath = args[2] ?? process.env.BILI_TEST_REPLACEMENT_LIBRARY;
const replacementLibrary = replacementPath ? resolve(replacementPath) : undefined;
const pluginVersion = (await readFile(new URL('../VERSION', import.meta.url), 'utf8')).trim();
const [major, minor, patch] = pluginVersion.match(/^([0-9]+)\.([0-9]+)\.([0-9]+)/).slice(1).map(Number);
const fixtureVersion = offset => `${major}.${minor}.${patch + offset}`;
const tmp = resolve(process.env.BILI_TEST_TMP ?? '.work/tmp');
await mkdir(tmp, { recursive: true });
const scratch = await mkdtemp(join(tmp, 'integration-'));
const children = [];
const requests = [];
const clientKey = 'synthetic-cpa-client';
const managementKey = 'synthetic-management';
const localPassword = 'synthetic-cpa-watchdog';
const upstreamKey = 'synthetic-cpa-provider';
const model = 'existing-cpa-model';
const target = 'gpt-5.4';
const foldMarker = 'SHOULD_BE_FOLDED_';
const foldSummary = 'The earlier test turns discussed workspace setup, model routing and synthetic results. The remaining turns verify that the new context keeps its recent messages.';
const preflightModel = 'claude-sonnet-4-5';
const preflightSummary = 'VISIBLE_PREFLIGHT: Earlier turns selected the synthetic batch size and verified the fixture.';
const hiddenReasoning = 'SYNTHETIC_REASONING_MUST_NOT_BE_USED_AS_A_SUMMARY';
let summaryMode;
let requestedFold = false;
let repair;
let streamTail;

const upstream = createServer(async (req, res) => {
	try {
		const chunks = [];
		for await (const chunk of req) chunks.push(chunk);
		const body = JSON.parse(Buffer.concat(chunks));
		requests.push({ path: req.url, auth: req.headers.authorization, engineToken: req.headers['x-omp-bili-access'], body });
		const base = { id: 'chatcmpl-smoke', created: 1, model: body.model };
		if (body.stream && JSON.stringify(body.messages).includes('TWO_NATIVE_CHOICES')) {
			res.writeHead(200, { 'content-type': 'text/event-stream' });
			const choices = ['lookup_alpha', 'lookup_beta'].map((name, index) => ({ index, delta: { role: 'assistant', tool_calls: [{ index: 0, id: 'c' + index, type: 'function', function: { name, arguments: JSON.stringify({ index }) } }] }, finish_reason: null }));
			res.write('data: ' + JSON.stringify({ ...base, object: 'chat.completion.chunk', choices }) + '\n\n');
			return res.end('data: ' + JSON.stringify({ ...base, object: 'chat.completion.chunk', choices: choices.map(c => ({ index: c.index, delta: {}, finish_reason: 'tool_calls' })) }) + '\n\ndata: [DONE]\n\n');
		}
		if (repair && !body.stream && body.tools?.length) {
			const attempt = ++repair.calls;
			if (attempt <= 2) {
				const summary = attempt === 1 && repair.kind === 'length' ? 'x'.repeat(21150) : foldSummary;
				const content = attempt === 1 && repair.kind === 'format' ? 'invalid range header' : JSON.stringify([{ startId: 'm00001', endId: 'm00004', summary }]);
				const tool = { id: `call_repair_${attempt}`, type: 'function', function: { name: 'compress', arguments: JSON.stringify({ content }) } };
				res.writeHead(200, { 'content-type': 'application/json' });
				return res.end(JSON.stringify({ ...base, object: 'chat.completion', usage: { prompt_tokens: 100, prompt_tokens_details: { cached_tokens: 80 }, completion_tokens: 1, total_tokens: 101 }, choices: [{ index: 0, message: { role: 'assistant', content: null, tool_calls: [tool] }, finish_reason: 'tool_calls' }] }));
			}
		}
		if (summaryMode && body.tools?.length && JSON.stringify(body.messages).length > 750000) {
			res.writeHead(400, { 'content-type': 'application/json' });
			return res.end(JSON.stringify({ error: { code: 'context_length_exceeded', type: 'invalid_request_error', message: 'The request exceeds the context window.' } }));
		}
		if (body.stream) {
			res.writeHead(200, { 'content-type': 'text/event-stream' });
			if (summaryMode && !body.tools?.length) {
				const chunks = [{ role: 'assistant', reasoning_content: hiddenReasoning }];
				if (summaryMode === 'visible') chunks.push({ content: preflightSummary });
				chunks.push({});
				for (const [i, delta] of chunks.entries()) {
					const choice = { index: 0, delta, finish_reason: i === chunks.length - 1 ? 'stop' : null };
					res.write('data: ' + JSON.stringify({ ...base, object: 'chat.completion.chunk', choices: [choice] }) + '\n\n');
				}
				return res.end('data: [DONE]\n\n');
			}
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
			if (summaryMode && !body.tools?.length) {
				return res.end(JSON.stringify({ ...base, object: 'chat.completion', choices: [{ index: 0, message: { role: 'assistant', content: summaryMode === 'json' ? preflightSummary : null, reasoning_content: hiddenReasoning }, finish_reason: 'stop' }] }));
			}
			res.end(JSON.stringify({ ...base, object: 'chat.completion', choices: [{ index: 0, message: { role: 'assistant', content: 'smoke-ok' }, finish_reason: 'stop' }], usage: { prompt_tokens: 5, prompt_tokens_details: { cached_tokens: 2 }, completion_tokens: 1, total_tokens: 6 } }));
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
	const child = spawn(executable, args, { cwd: join(scratch, 'server-cwd'), env, windowsHide: true, stdio: ['ignore', 'pipe', 'pipe'] });
	const record = { child, log: '', stopped: new Promise(done => child.once('close', (code, signal) => done({ code, signal }))) };
	child.on('error', error => { record.log += `${error}\n`; });
	for (const stream of [child.stdout, child.stderr]) stream.on('data', chunk => { record.log += chunk; });
	children.push(record);
	return record;
}

async function runtimeSnapshot(directory, prefix = '') {
	const files = [];
	const entries = await readdir(directory, { withFileTypes: true });
	for (const entry of entries.sort((left, right) => left.name.localeCompare(right.name))) {
		const path = join(directory, entry.name);
		const name = prefix + entry.name;
		assert(entry.isDirectory() || entry.isFile(), `Unexpected installed runtime entry ${name}`);
		if (entry.isDirectory()) {
			files.push({ name: `${name}/` });
			files.push(...await runtimeSnapshot(path, `${name}/`));
		} else {
			const info = await stat(path);
			const digest = createHash('sha256');
			for await (const chunk of createReadStream(path)) digest.update(chunk);
			files.push({ name, ino: info.ino, dev: info.dev, mtime: info.mtimeMs, size: info.size, mode: info.mode, sha256: digest.digest('hex') });
		}
	}
	return files;
}

async function instanceDirs(dataRoot) {
	try {
		return (await readdir(join(dataRoot, 'instances'))).sort();
	} catch (error) {
		if (error.code === 'ENOENT') return [];
		throw error;
	}
}

async function startHeartbeat(record, url) {
	const controller = new AbortController();
	const heartbeat = { controller, done: undefined, error: undefined };
	record.heartbeat = heartbeat;
	async function pulse() {
		assert(record.child.exitCode === null && record.child.signalCode === null, `CPA exited during keep-alive:\n${record.log}`);
		const response = await fetch(`${url}/keep-alive`, {
			headers: { 'X-Local-Password': localPassword },
			signal: AbortSignal.any([controller.signal, AbortSignal.timeout(2000)]),
		});
		assert.equal(response.status, 200, `Keep-alive rejected synthetic credentials: ${await response.text()}`);
	}
	for (let attempt = 0; ; attempt++) {
		try {
			await pulse();
			break;
		} catch (error) {
			if (error.cause?.code !== 'ECONNREFUSED' || attempt >= 119) throw error;
			await delay(250, undefined, { signal: controller.signal });
		}
	}
	heartbeat.done = (async () => {
		try {
			while (!controller.signal.aborted) {
				await delay(2000, undefined, { signal: controller.signal });
				await pulse();
			}
		} catch (error) {
			if (!controller.signal.aborted) heartbeat.error = error;
		}
	})();
}

async function stopHeartbeat(record) {
	record.heartbeat?.controller.abort();
	await record.heartbeat?.done;
}

async function waitForExit(record, milliseconds) {
	let timeout;
	try {
		return await Promise.race([
			record.stopped,
			new Promise((_, reject) => { timeout = setTimeout(() => reject(new Error(`CPA shutdown timed out:\n${record.log}`)), milliseconds); }),
		]);
	} finally {
		clearTimeout(timeout);
	}
}

async function stopCPA(record, dataRoot) {
	await stopHeartbeat(record);
	if (record.heartbeat.error) throw record.heartbeat.error;
	assert(record.child.exitCode === null && record.child.signalCode === null, `CPA exited before graceful shutdown:\n${record.log}`);
	const outcome = await waitForExit(record, 45000);
	assert.deepEqual(outcome, { code: 0, signal: null }, `CPA must exit successfully after watchdog shutdown:\n${record.log}`);
	for (let attempt = 0; attempt < 100; attempt++) {
		if ((await instanceDirs(dataRoot)).length === 0) return;
		await delay(50);
	}
	assert.deepEqual(await instanceDirs(dataRoot), [], 'CPA shutdown must remove its private engine instance');
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
	const pluginDir = join(scratch, 'custom-native-store');
	const dataRoot = join(pluginDir, 'omp-billion-context');
	const cpaPort = await freePort();
	const cpaUrl = `http://127.0.0.1:${cpaPort}`;
	const cpaConfig = join(scratch, 'cpa.yaml');
	await mkdir(pluginDir);
	await mkdir(join(scratch, 'server-cwd'));
	let activeLibrary = join(pluginDir, `omp-billion-context-v${pluginVersion}${extname(pluginPath)}`);
	await copyFile(pluginPath, activeLibrary);
	if (replacementLibrary) {
		const digest = async path => {
			const hash = createHash('sha256');
			for await (const chunk of createReadStream(path)) hash.update(chunk);
			return hash.digest('hex');
		};
		assert.notEqual(await digest(pluginPath), await digest(replacementLibrary), 'Replacement must be a distinct native image built from the same source');
	}
	await writeFile(cpaConfig, `host: 127.0.0.1\nport: ${cpaPort}\nauth-dir: ${JSON.stringify(join(scratch, 'auth'))}\napi-keys: [${clientKey}, synthetic-other-client]\nrouting:\n  strategy: fill-first\nremote-management:\n  secret-key: ${managementKey}\n  disable-control-panel: true\nplugins:\n  enabled: true\n  dir: ${JSON.stringify(pluginDir)}\n  configs:\n    omp-billion-context:\n      enabled: true\nopenai-compatibility:\n  - name: fake\n    base-url: ${upstreamUrl}/v1\n    api-key-entries:\n      - api-key: ${upstreamKey}\n      - api-key: synthetic-cpa-backup\n    models:\n      - name: ${target}\n        alias: ${model}\n      - name: ${preflightModel}\n`);
	const cpa = start(cpaPath, ['-config', cpaConfig, '-local-model', '-password', localPassword], {});
	await startHeartbeat(cpa, cpaUrl);
	const managementHeaders = { 'x-management-key': managementKey };
	const listing = await ready(`${cpaUrl}/v0/management/plugins`, cpa, managementHeaders);
	const entry = listing.plugins.find(plugin => plugin.id === 'omp-billion-context');
	assert.equal(entry?.registered, true, JSON.stringify(listing));
	assert.equal(entry.effective_enabled, true);
	assert.equal(entry.id, 'omp-billion-context');
	assert.equal(resolve(entry.path), activeLibrary, 'CPA must select the canonical versioned library path');
	assert.equal(basename(entry.path), `omp-billion-context-v${pluginVersion}${extname(pluginPath)}`);
	assert.equal(entry.metadata?.version, pluginVersion, 'Native metadata must match the canonical release version');
	const menu = entry.menus.find(item => item.menu === 'Billion Context');
	assert.equal(menu?.path, '/v0/resource/plugins/omp-billion-context/webui');
	const shell = await fetch(cpaUrl + menu.path, { signal: AbortSignal.timeout(3000) });
	assert.equal(shell.status, 200);
	console.log(JSON.stringify({ library: basename(entry.path), pluginID: entry.id, metadataVersion: entry.metadata.version, registered: entry.registered, effectiveEnabled: entry.effective_enabled, menu: menu.path, uiStatus: shell.status }));
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
	const engineDirs = () => instanceDirs(dataRoot);
	assert.deepEqual(await engineDirs(), [], 'Reading the shell and making denied requests must not start bili');
	await assert.rejects(stat(join(dataRoot, 'runtime')), { code: 'ENOENT' });

	const savedMarkers = async () => {
		const response = await readWebUI('/__bili/settings');
		assert.equal(response.status, 200, await response.clone().text());
		return (await response.json()).visibilityMarkers;
	};
	const setMarkers = async (visibilityMarkers, headers = managementHeaders) => bridge(bridgeTarget('/__bili/settings'), {
		method: 'PUT', headers: { ...headers, 'content-type': 'application/json' }, body: JSON.stringify({ visibilityMarkers }),
	});
	assert.equal((await setMarkers(false, {})).status, 401);
	assert.equal(await savedMarkers(), true);
	assert.equal((await setMarkers(false)).status, 200);
	assert.equal(await savedMarkers(), false);
	const initialOverview = await readWebUI('/__bili/overview');
	assert.equal(initialOverview.status, 200, await initialOverview.clone().text());
	assert.equal((await initialOverview.json()).sessions, 0);
	const runtimeParent = join(dataRoot, 'runtime');
	const installed = (await readdir(runtimeParent, { withFileTypes: true })).filter(entry => entry.isDirectory());
	assert.equal(installed.length, 1, 'Cold startup must publish one complete runtime');
	const runtimeRoot = join(runtimeParent, installed[0].name);
	const completion = JSON.parse(await readFile(join(runtimeRoot, 'version.json'), 'utf8'));
	assert.equal(completion.node_version, '24.22.3');
	const originalRuntime = await runtimeSnapshot(runtimeParent);
	assert(originalRuntime.some(file => /\/version\.json$/.test(file.name)));
	assert(originalRuntime.some(file => /\/node(?:\.exe)?$/.test(file.name)));
	assert(originalRuntime.some(file => file.name.endsWith('/kernel/worker.mjs')));
	assert(originalRuntime.some(file => file.name.endsWith('/node_modules/acp-kernel/package.json')));
	assert(!originalRuntime.some(file => file.name.includes('/node_modules/billion-context/')));
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

	// A client that sends no usable session header is still compressed: the
	// plugin identifies the conversation by its opening turn and keeps it scoped
	// per caller, model and protocol, so refusing here would leave such a client
	// uncompressed for good.
	const anonymous = await call('chat/completions', { model, max_tokens: 1000, messages }, { 'x-acp-session': '' });
	const anonymousBody = await anonymous.text();
	assert.equal(anonymous.status, 200, anonymousBody);
	assert.equal(requests.at(-1).body.model, target);
	assert(requests.at(-1).body.tools?.some(tool => ['acp_status', 'compress', 'acp_compress'].includes(tool.function?.name ?? tool.name)),
		'An unidentified conversation must still reach bili, which carries its own tools');
	const account = requests.at(-1).auth;
	assert([upstreamKey, 'synthetic-cpa-backup'].some(key => account === `Bearer ${key}`));

	const multi = await call('chat/completions', { model, n: 2, stream: true, messages: [{ role: 'user', content: 'TWO_NATIVE_CHOICES' }] });
	const choicesWire = await multi.text(); assert.equal(multi.status, 200, choicesWire);
	assert.match(choicesWire, /lookup_alpha/); assert.match(choicesWire, /lookup_beta/);
	assert.equal(requests.at(-1).body.tools?.length ?? 0, 0, 'Multiple choices must bypass the ACP executor entirely');

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

	// DSH reaches CPA over the Anthropic endpoint. Without this path the plugin
	// never sees the request and the provider rejects an oversized history.
	// call() already sends X-Acp-Session, so no second identifier is added:
	// two different values would make the request ambiguous and skip bili.
	// bili declares its compression tool in Anthropic shape, so the provider
	// receiving that tool is proof the request went through bili rather than
	// the ordinary CPA route.
	const claudeSession = 'smoke-claude';
	for (const stream of [false, true]) {
		let release;
		if (stream) streamTail = new Promise(done => { release = done; });
		try {
			const result = await call('messages', {
				model, max_tokens: 1000, system: 'smoke system prompt', stream,
				messages: [{ role: 'user', content: 'Synthetic conversation' }],
			}, { 'x-acp-session': claudeSession });
			assert.equal(result.status, 200);
			if (stream) {
				assert.match(result.headers.get('content-type'), /text\/event-stream/);
				const reader = result.body.getReader();
				const decoder = new TextDecoder();
				let prefix = '';
				while (!prefix.includes('smoke-ok')) {
					const chunk = await reader.read();
					assert(!chunk.done, 'The first Anthropic text must arrive before the provider finishes.');
					prefix += decoder.decode(chunk.value, { stream: true });
				}
				assert.match(prefix, /event: content_block_delta/, 'Anthropic frames need their event line');
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
		assert.equal(received.body.model, target);
		assert.equal(received.auth, account);
		const sessions = await (await readWebUI('/__bili/sessions?page=1&pageSize=20')).json();
		const anthropicSessions = (sessions.sessions ?? []).filter(item => item.protocol === 'claude').length;
		assert(anthropicSessions > 0, 'bili must record the Anthropic conversation');
		assert(received.body.tools?.some(tool => ['acp_status', 'compress', 'acp_compress'].includes(tool.name ?? tool.function?.name)),
			`An Anthropic request must carry bili's own tool, which proves it entered bili. tools=${JSON.stringify(received.body.tools?.map(tool => tool.name ?? tool.function?.name))}`);
		console.log(`messages, ${stream ? 'streaming' : 'JSON'}: CPA routed the Anthropic endpoint through bili`);
	}
	const opaque = await call('responses', { model, input: [{ type: 'reasoning', encrypted_content: 'opaque-fixture', summary: [] }, ...messages] });
	const opaqueBody = await opaque.text();
	assert.equal(opaque.status, 200, opaqueBody);
	assert.equal(requests.at(-1).body.tools?.length ?? 0, 0, 'Encrypted continuation must stay on the ordinary CPA route');
	const protectedInput = {
		model, reasoning: { context: 'current_turn' }, max_output_tokens: 1000, input: [
			...messages,
			{ type: 'reasoning', id: 'rs_fixture', encrypted_content: 'synthetic-blob', summary: [] },
			{ type: 'function_call', id: 'fc_fixture', call_id: 'call_fixture', name: 'lookup', arguments: '{}' },
			{ type: 'function_call_output', call_id: 'call_fixture', output: 'done' },
			{ role: 'user', content: 'Start the next turn.' },
		]
	};
	const priorSkips = async () => (await (await readWebUI('/bypass')).json()).formats?.['openai-response']?.previous_response_id ?? 0;
	const beforeDelta = await priorSkips();
	const protectedReply = await call('responses', protectedInput, { 'x-acp-session': 'protected-responses' });
	assert.equal(protectedReply.status, 200, await protectedReply.text());
	assert(requests.at(-1).body.tools?.some(tool => tool.function?.name === 'compress'), 'Identified full replay must enter bili through CPA');
	const delta = await call('responses', { ...protectedInput, previous_response_id: 'resp_fixture' });
	await delta.text();
	assert.equal(await priorSkips(), beforeDelta + 1, 'A server-side continuation must bypass the executor');
	console.log('CPA compressed an eligible Responses replay and kept its server-side continuation native');
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
	const counters = await overview.json();
	assert(counters.sessions > 0 && counters.cpaCompression, JSON.stringify(counters));
	const sessions = await readWebUI('/__bili/sessions?page=1&pageSize=20');
	assert.equal(sessions.status, 200);
	const sessionPage = await sessions.json();
	const session = sessionPage.sessions.find(item => !item.legacy);
	assert(session?.id, 'The native Sessions list must include a live session from the model requests');
	const detail = await readWebUI(`/__bili/sessions/${encodeURIComponent(session.id)}/detail`);
	assert.equal(detail.status, 200);
	assert.equal((await detail.json()).session.id, session.id);
	console.log('Native Overview and Sessions reflected CPA model requests');

	assert(requests.every(request => request.engineToken === undefined), 'Private worker state must not reach the provider');

	const history = Array.from({ length: 12 }, (_, index) => ({
		role: index % 2 ? 'assistant' : 'user',
		content: `Turn ${index}: ${(index < 4 ? `${foldMarker}${index} ` : 'Recent synthetic context ').repeat(150)}`,
	}));
	const beforeFold = requests.length;
	const folded = await call('chat/completions', { model, stream: true, messages: history, prompt_cache_key: 'existing-cache-key' }, { 'x-acp-session': 'smoke-compress' });
	const foldedText = await folded.text();
	assert.equal(folded.status, 200, foldedText);
	assert.match(foldedText, /smoke-ok/);
	assert(!foldedText.includes('[ACP]'), 'Disabling notifications must keep markers out of client output');
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
	const savedSessions = async () => {
		const response = await readWebUI('/__bili/sessions?page=1&pageSize=100');
		assert.equal(response.status, 200);
		return (await response.json()).sessions;
	};
	const foldedRows = (await savedSessions()).filter(row => row.blocks > 0);
	assert.equal(foldedRows.length, 1, 'Only the original caller folded this fixture');
	const foldedSessionId = foldedRows[0].id;
	assert(foldedSessionId);
	console.log('Bundled bili folded history; CPA preserved the account, cache key and prefix; caller state stayed isolated');

	for (const kind of ['format', 'length']) {
		const notifications = kind === 'length';
		assert.equal((await setMarkers(notifications)).status, 200);
		repair = { kind, calls: 0 };
		const start = requests.length;
		const result = await call('chat/completions', { model, messages: history }, { 'x-acp-session': 'smoke-repair-' + kind });
		const message = await result.text();
		assert.equal(result.status, 200, message);
		assert.equal(repair.calls, 3, 'One rejected tool, one repaired tool and one final answer');
		assert.match(message, /smoke-ok/);
		assert(!message.includes('call_repair_') && !message.includes('x'.repeat(100)));
		assert.equal(message.includes('[ACP]'), notifications, 'Only the successful marker is visible, and only when enabled');
		assert(JSON.stringify(requests[start + 2].body.messages).includes(foldSummary));
		assert(!JSON.stringify(requests[start + 2].body.messages).includes(`${foldMarker}1 `.repeat(10)));
		repair = undefined;
	}
	assert.equal((await setMarkers(false)).status, 200);
	const cacheStats = (await (await readWebUI('/__bili/overview')).json()).cache;
	assert(cacheStats.reported > 0 && cacheStats.reported <= cacheStats.requests);
	assert(cacheStats.readTokens > 0 && cacheStats.ratio > 0 && cacheStats.ratio <= 1);
	assert.equal(cacheStats.beforeFold?.ratio, 0.8, JSON.stringify(cacheStats));
	assert.equal(cacheStats.afterFold?.ratio, 0.4, JSON.stringify(cacheStats));
	console.log('The fixture reported cache usage before and after an actual fold; missing reports are not cache misses');

	async function eventually(check, message, milliseconds = 10000) {
		const deadline = Date.now() + milliseconds;
		do {
			if (await check()) return;
			await delay(50);
		} while (Date.now() < deadline);
		assert.fail(`${message}\n${cpa.log}`);
	}
	async function reconfigure(priority) {
		const response = await fetch(`${cpaUrl}/v0/management/plugins/omp-billion-context/config`, {
			method: 'PUT', headers: { ...managementHeaders, 'content-type': 'application/json' },
			body: JSON.stringify({ enabled: true, priority }), signal: AbortSignal.timeout(3000),
		});
		assert.equal(response.status, 200, await response.text());
		await eventually(async () => {
			const listing = await fetch(`${cpaUrl}/v8/management/plugins`, { headers: managementHeaders, signal: AbortSignal.timeout(2000) });
			assert.equal(listing.status, 200);
			const plugin = (await listing.json()).plugins.find(item => item.id === 'omp-billion-context');
			return plugin?.registered && plugin.effective_enabled && plugin.priority === priority;
		}, `CPA did not finish reconfigure with priority ${priority}`);
	}
	async function assertStoredFold() {
		const row = (await savedSessions()).find(item => item.id === foldedSessionId);
		assert(row, 'The historic session must be in the WebUI before the client sends any replay');
		const response = await call('chat/completions', { model, stream: true, messages: followup, prompt_cache_key: 'existing-cache-key' }, cycleHeaders);
		const text = await response.text();
		assert.equal(response.status, 200, text);
		assert.match(text, /smoke-ok/);
		const received = requests.at(-1).body;
		assert(JSON.stringify(received.messages).includes(foldSummary), 'The restored engine must send the previous summary');
		assert(!JSON.stringify(received.messages).includes(`${foldMarker}1 `.repeat(10)), 'The restored engine must omit the folded originals');
		assert.deepEqual(cacheFields(received), cacheFields(replay.body));
		const otherClient = await call('chat/completions', { model, stream: true, messages: followup, prompt_cache_key: 'existing-cache-key' }, { ...cycleHeaders, authorization: ['Bearer', 'synthetic-other-client'].join(' ') });
		assert.equal(otherClient.status, 200, await otherClient.text());
		assert(!JSON.stringify(requests.at(-1).body.messages).includes(foldSummary), 'Restoration must keep CPA callers isolated');
	}
	async function coldRecovery() {
		await assertStoredFold();
		assert.equal(await savedMarkers(), false);
		assert.deepEqual(await runtimeSnapshot(runtimeParent), originalRuntime, 'Recovery reuses the immutable runtime');
	}
	async function publishLibrary(version, source) {
		const staged = join(scratch, `replacement-${version}${extname(pluginPath)}`);
		if (source) await copyFile(source, staged);
		else await writeFile(staged, 'invalid native library fixture\n');
		const destination = join(pluginDir, `omp-billion-context-v${version}${extname(pluginPath)}`);
		await rename(staged, destination);
		return destination;
	}

	await reconfigure(11);
	const preserved = await call('chat/completions', { model, stream: true, messages: followup, prompt_cache_key: 'existing-cache-key' }, cycleHeaders);
	assert.equal(preserved.status, 200, await preserved.text());
	assert.equal(JSON.stringify(requests.at(-1).body.messages.slice(0, anchor + 1)), JSON.stringify(replay.body.messages.slice(0, anchor + 1)), 'Ordinary reconfigure must preserve the summary and provider prefix');
	assert.deepEqual(cacheFields(requests.at(-1).body), cacheFields(replay.body));

	let releaseBusy;
	streamTail = new Promise(done => { releaseBusy = done; });
	try {
		const held = await call('chat/completions', { model, messages, stream: true }, { 'x-acp-session': 'held-replacement-stream' });
		assert.equal(held.status, 200);
		const reader = held.body.getReader();
		const decoder = new TextDecoder();
		let text = '';
		while (!text.includes('smoke-ok')) {
			const chunk = await reader.read();
			assert(!chunk.done, 'Busy replacement fixture must hold a live stream');
			text += decoder.decode(chunk.value, { stream: true });
		}
		const logStart = cpa.log.length;
		const invalidBusy = await publishLibrary(fixtureVersion(1));
		const started = Date.now();
		await reconfigure(12);
		await eventually(() => cpa.log.slice(logStart).includes('plugin quiesce failed'), 'Held stream did not refuse replacement');
		assert(Date.now() - started < 6500, 'CPA must receive bounded busy refusal without canceling the stream');
		assert(!cpa.log.slice(logStart).includes('failed to load plugin'), 'Busy quiesce must abort before invalid-library loading');
		await rm(invalidBusy, { force: true });
		releaseBusy();
		for (; ;) {
			const chunk = await reader.read();
			if (chunk.done) break;
			text += decoder.decode(chunk.value, { stream: true });
		}
		assert.match(text, /\[DONE\]/, 'The refused replacement must let the original stream finish normally');
	} finally {
		releaseBusy?.();
		streamTail = undefined;
	}
	console.log('Ordinary reconfigure preserved the engine, summary and prefix; held streaming replacement returned busy and finished normally');

	const failureStart = cpa.log.length;
	const invalid = await publishLibrary(fixtureVersion(2));
	await reconfigure(13);
	await eventually(() => cpa.log.slice(failureStart).split('\n').some(line => line.includes('failed to load plugin omp-billion-context from') && line.includes(basename(invalid))), 'Invalid replacement did not fail in the native loader after quiesce');
	await rm(invalid, { force: true });
	await coldRecovery();
	console.log('Invalid-library loading failed after old quiesce; CPA rollback resumed a fresh lazy engine with complete client history');

	if (replacementLibrary) {
		const reloadStart = cpa.log.length;
		activeLibrary = await publishLibrary(fixtureVersion(3), replacementLibrary);
		await reconfigure(14);
		await eventually(() => cpa.log.slice(reloadStart).split('\n').some(line => line.includes('plugin hot reloaded') && line.includes(basename(activeLibrary))), 'Valid path-changing replacement did not become active');
		await coldRecovery();
		console.log('A distinct native image replaced the old path, released its engine and private instance, and reused the shared runtime');
	} else {
		console.log('Successful native-image replacement was not exercised; pass a distinct same-source replacement library as the third argument');
	}
	async function setEnabled(enabled) {
		const response = await fetch(`${cpaUrl}/v0/management/plugins/omp-billion-context/enabled`, {
			method: 'PATCH', headers: { ...managementHeaders, 'content-type': 'application/json' },
			body: JSON.stringify({ enabled }), signal: AbortSignal.timeout(3000),
		});
		assert.equal(response.status, 200, await response.text());
		let entry;
		await eventually(async () => {
			const listing = await fetch(`${cpaUrl}/v8/management/plugins`, { headers: managementHeaders, signal: AbortSignal.timeout(2000) });
			assert.equal(listing.status, 200);
			entry = (await listing.json()).plugins.find(item => item.id === 'omp-billion-context');
			return entry?.enabled === enabled && entry.registered === enabled && entry.effective_enabled === enabled;
		}, `CPA did not finish setting plugin enabled=${enabled}`);
		assert.equal(resolve(entry.path), activeLibrary, 'Disable/re-enable must keep the same selected library');
		return entry;
	}
	const disableLogStart = cpa.log.length;
	await setEnabled(false);
	await eventually(async () => (await engineDirs()).length === 0 && cpa.log.slice(disableLogStart).split('\n').some(line => line.includes('plugin unloaded') && line.includes(basename(activeLibrary))), 'Disable did not complete native shutdown and cleanup');
	const reenabled = await setEnabled(true);
	assert.equal(reenabled.metadata?.version, pluginVersion, 'Reinitialized native metadata must match VERSION');
	const reopenedShell = await fetch(cpaUrl + menu.path, { signal: AbortSignal.timeout(3000) });
	assert.equal(reopenedShell.status, 200, await reopenedShell.text());
	await coldRecovery();
	console.log('CPA disabled and re-enabled the same selected native image with a fresh host binding and lazy engine');
	assert.deepEqual(await runtimeSnapshot(runtimeParent), originalRuntime, 'Real requests must not modify the installed runtime');
	await stopCPA(cpa, dataRoot);
	assert.deepEqual(await runtimeSnapshot(runtimeParent), originalRuntime, 'Shutdown must retain the immutable runtime and stable lock');

	const [runtimeOS, runtimeArch] = completion.platform.split('-');
	const platformDir = join(pluginDir, runtimeOS, runtimeArch);
	await mkdir(platformDir, { recursive: true });
	await rename(activeLibrary, join(platformDir, basename(activeLibrary)));
	const restartConfig = await readFile(cpaConfig, 'utf8');
	await writeFile(cpaConfig, restartConfig.replace(
		`  dir: ${JSON.stringify(pluginDir)}\n`,
		'  dir: "../custom-native-store"\n',
	));
	const restarted = start(cpaPath, ['-config', cpaConfig, '-local-model', '-password', localPassword], {});
	await startHeartbeat(restarted, cpaUrl);
	const restartedListing = await ready(`${cpaUrl}/v0/management/plugins`, restarted, managementHeaders);
	assert.equal(restartedListing.plugins.find(plugin => plugin.id === 'omp-billion-context')?.registered, true);
	assert.deepEqual(await engineDirs(), [], 'Restart must not start an engine before its first supported request');
	await assertStoredFold();
	await assert.rejects(stat(join(platformDir, 'omp-billion-context')), { code: 'ENOENT' });
	assert.equal(await savedMarkers(), false);
	assert.deepEqual(await runtimeSnapshot(runtimeParent), originalRuntime, 'Restart must reuse the same marker, executable identity, mtimes and content');
	for (const stream of [false, true]) {
		const response = await call('chat/completions', { model, messages, stream }, { 'x-acp-session': 'restart-session' });
		const text = await response.text();
		assert.equal(response.status, 200, text);
		assert.match(text, /smoke-ok/);
		assert(requests.at(-1).body.tools?.length, 'The reused runtime must still compress supported requests');
	}
	for (summaryMode of ['json', 'visible', 'empty']) {
		const before = requests.length;
		const history = Array.from({ length: 211 }, (_, i) => ({
			role: i % 2 ? 'assistant' : 'user',
			content: Array.from({ length: 470 }, (_, j) => summaryMode + 's' + i + 'p' + j).join(' '),
		}));
		const reply = await call('messages', { model: preflightModel, stream: true, max_tokens: 8192, messages: history }, { 'x-acp-session': 'preflight-' + summaryMode });
		const text = await reply.text();
		const calls = requests.slice(before).map(r => r.body);
		const probes = calls.filter(b => !b.tools?.length);
		assert(probes.length > 0);
		const streamedProbes = probes.filter(b => b.stream);
		if (summaryMode === 'json') {
			assert.equal(streamedProbes.length, 0, 'A usable JSON summary must not trigger the alternate transport');
		} else {
			assert.equal(probes.filter(b => !b.stream).length, 1, 'Try the regular JSON summary once before switching');
			assert(streamedProbes.length > 0);
		}
		assert(!text.includes(hiddenReasoning), 'No private reasoning in the client response');
		const answers = calls.filter(b => b.tools?.length && JSON.stringify(b.messages).length <= 750000);
		if (summaryMode !== 'empty') {
			assert.equal(reply.status, 200, text);
			assert.match(text, /smoke-ok/);
			assert.equal(answers.length, 1);
			const historySent = JSON.stringify(answers[0].messages);
			assert(historySent.includes(preflightSummary) && historySent.includes(summaryMode + 's210p469'));
			assert(!historySent.includes(hiddenReasoning));
		} else {
			assert.equal(reply.status, 422, text);
			assert.equal(answers.length, 0, 'Do not dispatch oversized history without a summary');
			assert(streamedProbes.length <= 3, 'Stop once the engine confirms an unusable summary');
			assert.equal(new Set(streamedProbes.map(b => JSON.stringify(b.messages))).size, streamedProbes.length, 'Do not reissue an identical empty summary');
		}
	}
	summaryMode = undefined;
	console.log('Preflight accepted visible summary text and refused the reasoning-only response without forwarding the original history');
	assert.deepEqual(await runtimeSnapshot(runtimeParent), originalRuntime, 'Warm requests must leave installed runtime files unchanged');
	await stopCPA(restarted, dataRoot);
	assert.deepEqual(await engineDirs(), [], 'Both CPA starts must clean their private instances');
	assert.deepEqual(await runtimeSnapshot(runtimeParent), originalRuntime, 'The installed runtime and its lock remain unchanged after shutdown');
	console.log('Root and platform plugin locations reused one plugins-root runtime and removed their separate private instances');
} catch (error) {
	for (const { log } of children) if (log) console.error(log);
	throw error;
} finally {
	await Promise.all(children.map(stopHeartbeat));
	const cleanup = await Promise.allSettled(children.map(async record => {
		if (record.child.exitCode === null && record.child.signalCode === null) record.child.kill('SIGKILL');
		await waitForExit(record, 5000);
	}));
	upstream.closeAllConnections();
	if (upstream.listening) await new Promise(done => upstream.close(done));
	await rm(scratch, { recursive: true, force: true, maxRetries: 4 });
	for (const result of cleanup) if (result.status === 'rejected') throw result.reason;
}
