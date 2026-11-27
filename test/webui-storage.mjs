import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const html = readFileSync(new URL('../webui.html', import.meta.url), 'utf8');

const keyRemoval = /localStorage\.removeItem\(savedKey\)[\s\S]{0,120}sessionStorage\.removeItem\(savedKey\)/;
assert.match(html, keyRemoval, 'Locking must drop the pre-upgrade sessionStorage copy as well');

const matched = html.match(/addEventListener\('storage',\s*event\s*=>\s*\{([\s\S]*?)\}\);/);
assert.ok(matched, 'Cross-tab lock needs a storage listener');

const listenerAt = html.indexOf("addEventListener('storage'");
const openAt = html.indexOf('async function open(');
assert.ok(listenerAt > 0 && openAt > 0, 'Both the listener and open() must exist');
assert.ok(listenerAt < openAt, 'The lock listener must be installed before any open can start');

const savedKey = 'omp-billion-context.management-key';

// The shipped handler mutates the closure variables `key` and `openGeneration`,
// so the harness mirrors them onto the state object it passes in.
function dispatch(event, state) {
  const handler = new Function('event', 'savedKey', 'scope', 'location', `
    let key = scope.key;
    let openGeneration = scope.openGeneration;
    ${matched[1]}
    scope.key = key;
    scope.openGeneration = openGeneration;
  `);
  handler(event, savedKey, state, { reload: () => { state.reloaded += 1; } });
}

const unrelated = { key: 'live', openGeneration: 1, reloaded: 0 };
dispatch({ key: 'another-key', newValue: null }, unrelated);
assert.equal(unrelated.key, 'live', 'An unrelated key must not revoke access');
assert.equal(unrelated.reloaded, 0, 'An unrelated key must not reload the tab');

const freshLogin = { key: 'old', openGeneration: 1, reloaded: 0 };
dispatch({ key: savedKey, newValue: 'new-key' }, freshLogin);
assert.equal(freshLogin.key, 'old', 'A new value is a fresh login, not a lock');
assert.equal(freshLogin.reloaded, 0, 'A fresh login must not reload another tab');

const locked = { key: 'live', openGeneration: 1, reloaded: 0 };
dispatch({ key: savedKey, newValue: null }, locked);
assert.equal(locked.key, '', 'A lock in another tab must clear the key');
assert.equal(locked.reloaded, 1, 'The locked tab must reload to drop rendered data');
assert.equal(locked.openGeneration, 2, 'A pending open must be invalidated by the lock');

// Every await point in open() must re-check the generation before it can store
// the key or paint the authenticated UI.
const openBody = html.slice(openAt);
const guards = openBody.match(/if \(generation !== openGeneration\) return;/g) ?? [];
assert.ok(guards.length >= 4, `open() needs a generation guard after each await, found ${guards.length}`);
assert.match(openBody, /if \(generation !== openGeneration\) return;\s*try \{localStorage\.setItem\(savedKey, candidate\);/,
  'The key must not be stored by an open that a lock invalidated');

console.log('Cross-tab lock clears the key, invalidates pending opens, and reloads; unrelated and fresh-login events are ignored');
