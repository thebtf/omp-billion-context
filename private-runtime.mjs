import { Server } from 'node:http';
import { timingSafeEqual } from 'node:crypto';

const token = Buffer.from(process.env.OMP_ENGINE_TOKEN);
const proof = process.env.OMP_ENGINE_PROOF;
// The plugin's own leash on a stopped engine is shorter than this timer, so it
// only decides the case where the plugin is gone and the shutdown below never
// finishes.
const exitDeadlineMs = 20000;
delete process.env.OMP_ENGINE_TOKEN;
delete process.env.OMP_ENGINE_PROOF;

const emit = Server.prototype.emit;
Server.prototype.emit = function (event, ...args) {
  if (event === 'upgrade' || event === 'connect') {
    args[1].destroy();
    return true;
  }
  if (event === 'request') {
    const [request, response] = args;
    if (request.method === 'GET' && request.url === '/__bili/health') {
      response.setHeader('x-omp-bili-proof', proof);
    } else {
      const supplied = Buffer.from(request.headers['x-omp-bili-access'] ?? '');
      if (supplied.length !== token.length || !timingSafeEqual(token, supplied)) {
        response.writeHead(403).end();
        return true;
      }
      delete request.headers['x-omp-bili-access'];
      for (let i = request.rawHeaders.length - 2; i >= 0; i -= 2) {
        if (request.rawHeaders[i].toLowerCase() === 'x-omp-bili-access') request.rawHeaders.splice(i, 2);
      }
    }
  }
  return Reflect.apply(emit, this, [event, ...args]);
};

// bili performs its own durable write per session envelope and flushes the
// pending ones during an orderly shutdown: it stops accepting connections,
// drains, then flushes every session, and it registers SIGTERM/SIGINT handlers
// for exactly that. Closing stdin therefore requests that shutdown instead of
// exiting at once and dropping the last fold. A build without a handler gets the
// immediate exit.
let leaving = false;
function parentClosed() {
  if (leaving) return;
  leaving = true;
  setTimeout(() => process.exit(0), exitDeadlineMs);
  if (!process.emit('SIGTERM')) process.exit(0);
}
process.stdin.once('end', parentClosed);
process.stdin.once('error', parentClosed);
process.stdin.resume();
