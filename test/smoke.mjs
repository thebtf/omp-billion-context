import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { copyFile, mkdir, mkdtemp, readFile, readdir, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { basename, join, resolve } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';

const args = process.argv.slice(2);
assert.equal(args.length, 2, 'Usage: node test/smoke.mjs <cpa-executable> <plugin-library>');
const [cpaPath, pluginPath] = args.map(path => resolve(path));
const tmp = resolve('.work', 'tmp');
await mkdir(tmp, { recursive: true });
const scratch = await mkdtemp(join(tmp, 'integration-'));
const children = [];
const requests = [];
const clientKey = 'synthetic-cpa-client';
const upstreamKey = 'synthetic-upstream';
const alias = 'bili-smoke';
const target = 'gpt-5.4';
const foldMarker = 'SHOULD_BE_FOLDED_';
const foldSummary = 'The earlier test turns discussed workspace setup, model routing and synthetic results. The remaining turns verify that the new context keeps its recent messages.';
let requestedFold = false;

const upstream = createServer(async (req, res) => {
	try {
		const chunks = [];
		for await (const chunk of req) chunks.push(chunk);
		const body = JSON.parse(Buffer.concat(chunks));
		requests.push({ path: req.url, auth: req.headers.authorization, engineToken: req.headers['x-omp-bili-access'], body });
		const base = { id: 'chatcmpl-smoke', created: 1, model: body.model };
		if (req.url === '/v1/responses') {
			const response = {
				id: 'resp_smoke', object: 'response', created_at: 1, model: body.model,
				status: 'completed', output: [{ id: 'msg_smoke', type: 'message', role: 'assistant', status: 'completed', content: [{ type: 'output_text', text: 'smoke-ok', annotations: [] }] }],
				usage: { input_tokens: 5, output_tokens: 1, total_tokens: 6 },
			};
			if (body.stream) {
				res.writeHead(200, { 'content-type': 'text/event-stream' });
				if (!requestedFold && JSON.stringify(body.input).includes(foldMarker)) {
					requestedFold = true;
					const item = { type: 'function_call', id: 'fc_smoke', call_id: 'call_smoke', name: 'compress' };
					const args = JSON.stringify({ content: [{ startId: 'm00001', endId: 'm00004', summary: foldSummary }] });
					for (const [type, data] of [
						['response.output_item.added', { item, output_index: 0 }],
						['response.function_call_arguments.delta', { item_id: item.id, delta: args }],
						['response.output_item.done', { item: { ...item, arguments: args }, output_index: 0 }],
						['response.completed', { response: { ...response, output: [] } }],
					]) res.write(`event: ${type}\ndata: ${JSON.stringify({ type, ...data })}\n\n`);
					res.end();
					return;
				}
				res.write(`event: response.output_text.delta\ndata: ${JSON.stringify({ type: 'response.output_text.delta', item_id: 'msg_smoke', output_index: 0, content_index: 0, delta: 'smoke-ok' })}\n\n`);
				res.end(`event: response.completed\ndata: ${JSON.stringify({ type: 'response.completed', response })}\n\n`);
			} else {
				res.writeHead(200, { 'content-type': 'application/json' });
				res.end(JSON.stringify(response));
			}
		} else if (body.stream) {
			res.writeHead(200, { 'content-type': 'text/event-stream' });
			res.write(`data: ${JSON.stringify({ ...base, object: 'chat.completion.chunk', choices: [{ index: 0, delta: { role: 'assistant', content: 'smoke-ok' }, finish_reason: null }] })}\n\n`);
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
	await writeFile(cpaConfig, `host: 127.0.0.1\nport: ${cpaPort}\nauth-dir: ${JSON.stringify(join(scratch, 'auth'))}\napi-keys: [${clientKey}, synthetic-other-client]\nremote-management:\n  secret-key: synthetic-management\n  disable-control-panel: true\nplugins:\n  enabled: true\n  dir: ${JSON.stringify(pluginDir)}\n  configs:\n    omp-billion-context:\n      enabled: true\nopenai-compatibility:\n  - name: fake\n    base-url: ${upstreamUrl}/v1\n    api-key-entries:\n      - api-key: synthetic-direct\n    models:\n      - name: direct-model\n`);
	const cpa = start(cpaPath, ['-config', cpaConfig, '-local-model'], {
		OMP_BILI_UPSTREAM_URL: upstreamUrl,
		OMP_BILI_MODEL_ALIAS: alias, OMP_BILI_TARGET_MODEL: target,
		OMP_BILI_CLIENT_TOKEN: clientKey, OMP_BILI_UPSTREAM_TOKEN: upstreamKey,
	});
	const listing = await ready(`${cpaUrl}/v0/management/plugins`, cpa, { 'x-management-key': 'synthetic-management' });
	const entry = listing.plugins.find(plugin => plugin.id === 'omp-billion-context');
	assert.equal(entry?.registered, true, JSON.stringify(listing));
	assert.equal(entry.effective_enabled, true);
	console.log('CPA registered and enabled the native library');

	const messages = [{ role: 'user', content: 'Synthetic conversation' }];
	function call(path, body, headers = {}) {
		return fetch(`${cpaUrl}/v1/${path}`, {
			method: 'POST', body: JSON.stringify(body), signal: AbortSignal.timeout(35000),
			headers: { 'content-type': 'application/json', authorization: `Bearer ${clientKey}`, 'x-acp-session': 'smoke-session', ...headers },
		});
	}
	const noSession = await call('chat/completions', { model: alias, messages }, { 'x-acp-session': '' });
	assert.equal(noSession.status, 400);
	await noSession.text();
	const unauthorized = await call('chat/completions', { model: alias, messages }, { authorization: ['Bearer', 'synthetic-other-client'].join(' ') });
	assert.equal(unauthorized.status, 401);
	await unauthorized.text();
	assert.equal(requests.length, 0, 'Rejected requests must not reach the backend');

	const regular = await call('chat/completions', { model: 'direct-model', messages });
	const regularBody = await regular.text();
	assert.equal(regular.status, 200, regularBody);
	assert.equal(requests.at(-1).auth, 'Bearer synthetic-direct');
	assert.equal(requests.at(-1).body.model, 'direct-model');

	for (const [path, input] of [['chat/completions', { messages }], ['responses', { input: messages }]]) {
		for (const stream of [false, true]) {
			const result = await call(path, { model: alias, ...input, stream });
			const text = await result.text();
			assert.equal(result.status, 200, text);
			assert.match(text, /smoke-ok/);
			if (stream) assert.match(result.headers.get('content-type'), /text\/event-stream/);
			const received = requests.at(-1);
			assert.equal(received.path, `/v1/${path}`);
			assert.equal(received.body.model, target);
			assert.equal(received.auth, `Bearer ${upstreamKey}`);
			console.log(`${path}, ${stream ? 'streaming' : 'JSON'}: backend saw target model and separate token`);
		}
	}
	assert(requests.some(({ body }) => body.tools?.some(tool => ['acp_status', 'compress', 'acp_compress'].includes(tool.function?.name ?? tool.name))), 'The real billion-context process should inject context management tools');
	console.log('billion-context injected context management tools; unrelated model bypassed it');

	const engines = (await readdir(scratch)).filter(name => name.startsWith('omp-billion-context-'));
	assert.equal(engines.length, 1, 'CPA must start exactly one bundled engine');
	const engineConfig = JSON.parse(await readFile(join(scratch, engines[0], 'bili.json'), 'utf8'));
	const unauthorizedEngine = await fetch(`http://127.0.0.1:${engineConfig.port}/__bili/stats`);
	assert.equal(unauthorizedEngine.status, 403);
	await unauthorizedEngine.text();
	assert(requests.every(request => request.engineToken === undefined), 'The engine access token must not reach the backend');

	const history = Array.from({ length: 12 }, (_, index) => ({
		type: 'message', role: index % 2 ? 'assistant' : 'user',
		content: `Turn ${index}: ${(index < 4 ? `${foldMarker}${index} ` : 'Recent synthetic context ').repeat(150)}`,
	}));
	const beforeFold = requests.length;
	const folded = await call('responses', { model: alias, stream: true, input: history }, { 'x-acp-session': 'smoke-compress' });
	const foldedText = await folded.text();
	assert.equal(folded.status, 200, foldedText);
	assert.match(foldedText, /smoke-ok/);
	assert(!foldedText.includes('call_smoke'), 'The client must not execute the compression tool');
	assert(requestedFold);
	const replay = requests.slice(beforeFold + 1).find(({ body }) => JSON.stringify(body.input).includes(foldSummary));
	assert(replay, 'billion-context must replay the request with the model-generated summary');
	const replayInput = JSON.stringify(replay.body.input);
	for (let index = 1; index < 4; index++) {
		assert(!replayInput.includes(`${foldMarker}${index} `.repeat(10)), `The replay must omit the full text of folded turn ${index}`);
	}
	assert(replayInput.includes('Turn 11:'), 'The replay must preserve the recent conversation');
	console.log('billion-context folded old messages and replayed with the summary and recent history');
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
