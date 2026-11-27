import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const html = readFileSync(new URL('../webui.html', import.meta.url), 'utf8');

const keyRemoval = /localStorage\.removeItem\(savedKey\)[\s\S]{0,120}sessionStorage\.removeItem\(savedKey\)/;
assert.match(html, keyRemoval, 'Locking must drop the pre-upgrade sessionStorage copy as well');

const matched = html.match(/addEventListener\('storage',\s*event\s*=>\s*\{([\s\S]*?)\}\);/);
assert.ok(matched, 'Cross-tab lock needs a storage listener');

const savedKey = 'omp-billion-context.management-key';

// The shipped handler reads and clears the closure variable `key`, so the
// harness shadows it with an object property of the same name.
function dispatch(event, state) {
  const handler = new Function('event', 'savedKey', 'scope', 'location', `
    let key = scope.key;
    ${matched[1]}
    scope.key = key;
  `);
  handler(event, savedKey, state, { reload: () => { state.reloaded += 1; } });
}

const unrelated = { key: 'live', reloaded: 0 };
dispatch({ key: 'another-key', newValue: null }, unrelated);
assert.equal(unrelated.key, 'live', 'An unrelated key must not revoke access');
assert.equal(unrelated.reloaded, 0, 'An unrelated key must not reload the tab');

const freshLogin = { key: 'old', reloaded: 0 };
dispatch({ key: savedKey, newValue: 'new-key' }, freshLogin);
assert.equal(freshLogin.key, 'old', 'A new value is a fresh login, not a lock');
assert.equal(freshLogin.reloaded, 0, 'A fresh login must not reload another tab');

const locked = { key: 'live', reloaded: 0 };
dispatch({ key: savedKey, newValue: null }, locked);
assert.equal(locked.key, '', 'A lock in another tab must clear the key');
assert.equal(locked.reloaded, 1, 'The locked tab must reload to drop rendered data');

console.log('Cross-tab lock clears the key and reloads; unrelated and fresh-login events are ignored');
