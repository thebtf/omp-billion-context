import { once } from 'node:events';
import { createHost } from './host.mjs';

const limit = 64 * 1024 * 1024;
if (!process.argv[2]) throw new Error('Session directory is required');
const dispatch = createHost(process.argv[2]);

async function reply(message) {
	let line = JSON.stringify(message);
	if (Buffer.byteLength(line) > limit) line = JSON.stringify({ id: message.id, ok: false, error: 'Kernel result too large' });
	if (!process.stdout.write(line + '\n')) await once(process.stdout, 'drain');
}

async function handle(bytes) {
	let request;
	try { request = JSON.parse(bytes.toString('utf8')); } catch { throw new Error('Invalid kernel frame'); }
	if (!Number.isSafeInteger(request?.id)) throw new Error('Invalid kernel request ID');
	try {
		const result = request.op === 'ping' ? { ready: true } : await dispatch(request);
		await reply({ id: request.id, ok: true, result });
	} catch {
		await reply({ id: request.id, ok: false, error: 'Kernel operation failed' });
	}
}

const active = new Set();
let chunks = [], size = 0;
for await (const data of process.stdin) {
	let offset = 0;
	while (offset < data.length) {
		const newline = data.indexOf(10, offset);
		const end = newline < 0 ? data.length : newline;
		const slice = data.subarray(offset, end);
		size += slice.length;
		if (size > limit) throw new Error('Kernel input too large');
		chunks.push(slice);
		if (newline < 0) break;
		if (active.size >= 8) await Promise.race(active);
		const work = handle(chunks.length === 1 ? slice : Buffer.concat(chunks, size));
		active.add(work); work.then(() => active.delete(work), () => active.delete(work));
		chunks = []; size = 0; offset = newline + 1;
	}
}
await Promise.all(active);
if (size) process.exitCode = 1;
