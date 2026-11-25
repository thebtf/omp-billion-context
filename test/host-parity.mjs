import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { copyFile, mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { join, resolve } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';

assert.equal(process.argv.length, 5, 'Usage: node test/host-parity.mjs <CPA executable> <fallback library> <tracing library>');
const [cpa, fallback, tracing] = process.argv.slice(2).map(file => resolve(file));
const temporary = resolve(process.env.BILI_TEST_TMP ?? '.work/tmp');
await mkdir(temporary, { recursive: true });
const scratch = await mkdtemp(join(temporary, 'host-parity-'));
const clientKeys = ['synthetic-client-one', 'synthetic-client-two'];
const management = { 'x-management-key': 'synthetic-management-key' };
const prompt = 'Stable synthetic reference material. '.repeat(200);
const tracePath = '/v0/management/plugins/llm-tracing/traces';

async function listen(server) {
	server.listen(0, '127.0.0.1');
	await once(server, 'listening');
	return server.address().port;
}

async function close(server) {
	server.closeAllConnections();
	await new Promise((done, fail) => server.close(error => error ? fail(error) : done()));
}

function childEnv(dir) {
	const safe = Object.fromEntries(Object.entries(process.env).filter(([key]) => /^(SystemRoot|WINDIR|ComSpec|LANG|TZ)$/i.test(key)));
	return { ...safe, HOME: dir, USERPROFILE: dir, APPDATA: dir, LOCALAPPDATA: dir, XDG_CONFIG_HOME: dir, XDG_STATE_HOME: dir, TEMP: dir, TMP: dir, TMPDIR: dir, PATH: '' };
}

async function waitJSON(url, accept, child) {
	for (let attempt = 0; attempt < 80 && child.exitCode === null; attempt++) {
		try {
			const response = await fetch(url, { headers: management, signal: AbortSignal.timeout(1000) });
			if (response.ok) {
				const data = await response.json();
				if (accept(data)) return data;
			}
		} catch { }
		await delay(100);
	}
	throw new Error(`Required evidence not ready: ${url}`);
}

async function runArm(label, callback) {
	const observations = [];
	const origin = createServer(async (req, res) => {
		try {
			const chunks = [];
			for await (const chunk of req) chunks.push(chunk);
			const body = JSON.parse(Buffer.concat(chunks));
			const account = req.headers.authorization?.split(' ').at(-1);
			const marker = body.messages.at(-1).content;
			const fields = {
				path: req.url, model: body.model, account, stream: !!body.stream, marker,
				cacheKey: body.prompt_cache_key ?? null, session: req.headers.session_id ?? null,
				prefix: JSON.stringify(body.messages.slice(0, -1)), tools: JSON.stringify(body.tools ?? [])
			};
			observations.push(fields);
			if (marker === 'retry' && !observations.slice(0, -1).some(item => item.marker === 'retry')) {
				res.writeHead(429, { 'content-type': 'application/json', 'retry-after': '1' });
				return res.end(JSON.stringify({ error: { message: 'synthetic rate limit', type: 'rate_limit_error' } }));
			}
			const base = { id: 'chatcmpl-fixture', created: 1, model: body.model };
			const usage = { prompt_tokens: 2020, completion_tokens: 5, total_tokens: 2025, prompt_tokens_details: { cached_tokens: 2000 } };
			if (body.stream) {
				res.writeHead(200, { 'content-type': 'text/event-stream' });
				for (const text of ['fixture-', 'reply']) {
					const chunk = { ...base, object: 'chat.completion.chunk', choices: [{ index: 0, delta: { content: text }, finish_reason: null }] };
					res.write(`data: ${JSON.stringify(chunk)}\n\n`);
				}
				const end = { ...base, object: 'chat.completion.chunk', choices: [{ index: 0, delta: {}, finish_reason: 'stop' }], usage };
				return res.end(`data: ${JSON.stringify(end)}\n\ndata: [DONE]\n\n`);
			}
			res.writeHead(200, { 'content-type': 'application/json' });
			res.end(JSON.stringify({ ...base, object: 'chat.completion', usage, choices: [{ index: 0, finish_reason: 'stop', message: { role: 'assistant', content: 'fixture-reply' } }] }));
		} catch {
			res.writeHead(500).end();
		}
	});
	const originPort = await listen(origin);
	let service;
	let stopped;
	let logs = '';
	try {
		const dir = join(scratch, label);
		const plugins = join(dir, 'plugins');
		await mkdir(plugins, { recursive: true });
		const suffix = process.platform === 'win32' ? 'dll' : process.platform === 'darwin' ? 'dylib' : 'so';
		await copyFile(tracing, join(plugins, `llm-tracing.${suffix}`));
		if (callback) await copyFile(fallback, join(plugins, `model-fallback-router.${suffix}`));
		const portFinder = createServer();
		const port = await listen(portFinder);
		await close(portFinder);
		const base = `http://127.0.0.1:${port}`;
		const config = join(dir, 'cpa.yaml');
		await writeFile(config, `host: 127.0.0.1\nport: ${port}\nauth-dir: ${JSON.stringify(join(dir, 'auth'))}\napi-keys: ${JSON.stringify(clientKeys)}\nremote-management:\n  secret-key: ${management['x-management-key']}\n  disable-control-panel: true\nobservability:\n  usage:\n    usage-statistics-enabled: true\nrouting:\n  strategy: round-robin\n  session-affinity: true\nplugins:\n  enabled: true\n  dir: ${JSON.stringify(plugins)}\n  configs:\n    llm-tracing:\n      enabled: true\n      save_history: false\n      completion_grace_ms: 100\n${callback ? `    model-fallback-router:\n      enabled: true\n      rules:\n        - name: callback-witness\n          models: [callback-witness]\n          primary_model: cpa-existing\n          fallback_models: [cpa-existing]\n        - name: same-model\n          models: [cpa-existing]\n          primary_model: '$requested'\n          fallback_models: ['$requested']\n` : ''}openai-compatibility:\n  - name: synthetic\n    base-url: http://127.0.0.1:${originPort}/v1\n    api-key-entries:\n      - api-key: synthetic-account-a\n      - api-key: synthetic-account-b\n    models:\n      - name: provider-model\n        alias: cpa-existing\n`);
		service = spawn(cpa, ['-config', config, '-local-model'], { cwd: dir, env: childEnv(dir), windowsHide: true, stdio: ['ignore', 'pipe', 'pipe'] });
		stopped = new Promise(done => service.once('close', done));
		service.on('error', error => { logs += String(error); });
		for (const stream of [service.stdout, service.stderr]) stream.on('data', data => { logs = (logs + data).slice(-8000); });
		await waitJSON(`${base}/v0/management/plugins`, body => {
			const names = callback ? ['llm-tracing', 'model-fallback-router'] : ['llm-tracing'];
			return names.every(id => body.plugins.some(plugin => plugin.id === id && plugin.effective_enabled));
		}, service);

		async function send(marker, { key = 0, stream = false, model = 'cpa-existing' } = {}) {
			return fetch(`${base}/v1/chat/completions`, {
				method: 'POST', signal: AbortSignal.timeout(10000),
				headers: { connection: 'close', 'content-type': 'application/json', authorization: `Bearer ${clientKeys[key] ?? 'bad-fixture-key'}`, 'x-session-id': `fixture-session-${key}` },
				body: JSON.stringify({
					model, stream, prompt_cache_key: `fixture-cache-${key}`,
					messages: [{ role: 'system', content: prompt }, { role: 'user', content: marker }],
					tools: [{ type: 'function', function: { name: 'fixture_tool', parameters: { type: 'object', properties: {} } } }]
				}),
			});
		}
		const rejected = await fetch(`${base}/v1/chat/completions`, {
			method: 'POST', headers: { connection: 'close', authorization: 'Bearer bad-fixture-key' },
			signal: AbortSignal.timeout(10000),
		});
		assert.equal(rejected.status, 401);
		await rejected.text();
		assert.equal(observations.length, 0);
		for (const [marker, options] of [['seed-a', {}], ['seed-b', { key: 1 }], ['growth-a', {}], ['growth-b', { key: 1 }], ['stream', { stream: true }], ['retry', {}]]) {
			const reply = await send(marker, options);
			const text = await reply.text();
			assert.equal(reply.status, 200, `${label}/${marker}: ${text}`);
			if (options.stream) {
				assert.match(text, /fixture-/);
				assert.match(text, /reply/);
				assert.equal((text.match(/\[DONE\]/g) ?? []).length, 1);
			} else {
				assert.equal(JSON.parse(text).choices[0].message.content, 'fixture-reply');
			}
		}
		const receipt = await waitJSON(`${base}${tracePath}`, data => data.traces?.filter(trace => trace.requested_model === 'cpa-existing' && trace.attempt_count > 0).length >= 6, service);
		const beforeWitness = observations.length;
		const witness = await send('witness', { model: 'callback-witness' });
		assert.equal(witness.ok, callback, `${label}: callback-only witness`);
		await witness.text();
		assert.equal(observations.length, beforeWitness + Number(callback));
		const traces = receipt.traces.filter(trace => trace.requested_model === 'cpa-existing');
		return { origin: observations.slice(0, beforeWitness), traces };
	} catch (error) {
		console.error(logs);
		throw error;
	} finally {
		if (service) {
			service.kill();
			await stopped;
		}
		await close(origin);
	}
}

try {
	const direct = await runArm('direct', false);
	const callback = await runArm('callback', true);
	function summarize(arm) {
		return {
			origin: arm.origin.map(({ prefix, tools, ...item }) => ({ ...item, prefixBytes: Buffer.byteLength(prefix), toolBytes: Buffer.byteLength(tools) })),
			traces: arm.traces.map(trace => ({ model: trace.requested_model, stream: trace.stream, attempts: trace.attempt_count, cached: trace.cache_read_tokens })),
		};
	}
	function normalized(value) {
		if (Array.isArray(value)) return value.map(normalized);
		if (value !== null && typeof value === 'object') {
			return Object.fromEntries(Object.keys(value).sort().map(key => [key, normalized(value[key])]));
		}
		return value;
	}
	const mismatches = [];
	for (const marker of ['seed-a', 'seed-b', 'growth-a', 'growth-b', 'stream', 'retry']) {
		const a = direct.origin.filter(item => item.marker === marker);
		const b = callback.origin.filter(item => item.marker === marker);
		assert(a.length && b.length, `Both origins must see ${marker}`);
		for (const field of ['path', 'model', 'cacheKey', 'session', 'prefix', 'tools', 'stream']) {
			assert(a.every(item => item[field] === a[0][field]) && b.every(item => item[field] === b[0][field]), `Unstable ${marker}/${field} on retry`);
			if (a[0][field] !== b[0][field]) {
				const isJSON = field === 'prefix' || field === 'tools';
				const orderOnly = isJSON && JSON.stringify(normalized(JSON.parse(a[0][field]))) === JSON.stringify(normalized(JSON.parse(b[0][field])));
				mismatches.push(`${marker}:${field}${orderOnly ? ':json-key-order' : ''}`);
			}
		}
		if (a.length !== b.length) mismatches.push(`${marker}:attempts`);
	}
	function affinity(arm) {
		const account = marker => arm.origin.find(item => item.marker === marker).account;
		return ['growth-a', 'stream', 'retry'].every(marker => account(marker) === account('seed-a')) && account('growth-b') === account('seed-b');
	}
	if (!affinity(direct) || !affinity(callback)) mismatches.push('credential-affinity');
	for (const [name, arm] of Object.entries({ direct, callback })) {
		const billed = arm.traces.reduce((sum, trace) => sum + trace.attempt_count, 0);
		if (billed !== arm.origin.length) mismatches.push(`${name}:reported-attempts`);
		if (arm.traces.length !== 6) mismatches.push(`${name}:trace-count`);
	}
	console.log(JSON.stringify({ verdict: mismatches.length ? 'mismatch' : 'parity-observed', mismatches, direct: summarize(direct), callback: summarize(callback) }, null, 2));
} finally {
	await rm(scratch, { recursive: true, force: true, maxRetries: 5 });
}
