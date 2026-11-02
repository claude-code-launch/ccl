'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '../internal/proxymanager/codex_refresh.jxa'), 'utf8');

// Exercise the real JXA control flow without touching running applications.
function refresh(apps, openStatus = 0) {
  let now = 0;
  const events = [];
  const task = {
    get launch() { events.push({path: this.launchPath, args: this.arguments}); },
    get waitUntilExit() { return undefined; },
    terminationStatus: openStatus
  };
  const bridge = value => value;
  Object.assign(bridge, {
    NSWorkspace: {sharedWorkspace: {runningApplications: {count: apps.length, objectAtIndex: index => apps[index]}}},
    NSApplicationActivationPolicyRegular: 0,
    NSThread: {sleepForTimeInterval: seconds => { now += seconds * 1000; }},
    NSTask: {alloc: {init: task}}
  });
  const context = vm.createContext({$: bridge, ObjC: {import() {}, unwrap: value => value}, Date: {now: () => now}});
  vm.runInContext(source, context);
  let error;
  try { context.run(); } catch (caught) { error = caught; }
  // Convert arrays out of the vm realm for portable deep equality.
  return {events: JSON.parse(JSON.stringify(events)), error};
}

function app(options = {}) {
  const state = {quits: 0};
  const value = {
    bundleIdentifier: 'com.openai.codex', activationPolicy: 0,
    bundleURL: {path: '/Applications/Codex name "quoted".app'},
    get terminate() { state.quits++; return !options.refuse; },
    get isTerminated() { return !options.pending; },
    get forceTerminate() { throw new Error('force termination must never be used'); }
  };
  return {value: Object.assign(value, options.properties), state};
}

test('Codex refresh quits normally and reopens the same bundle with argv', () => {
  const fixture = app();
  const result = refresh([fixture.value]);
  assert.equal(result.error, undefined);
  assert.equal(fixture.state.quits, 1);
  assert.deepEqual(result.events, [{path: '/usr/bin/open', args: ['-a', fixture.value.bundleURL.path]}]);
});

test('absent Codex and unrelated applications are left alone', () => {
  const unrelated = app({properties: {bundleIdentifier: 'com.openai.chat'}});
  const helper = app({properties: {bundleIdentifier: 'com.openai.codex.helper', activationPolicy: 1}});
  for (const apps of [[], [unrelated.value, helper.value]]) {
    const result = refresh(apps);
    assert.equal(result.error, undefined);
    assert.deepEqual(result.events, []);
  }
  assert.equal(unrelated.state.quits + helper.state.quits, 0);
});

test('a refused or pending quit does not force termination or open a second app', () => {
  for (const options of [{refuse: true}, {pending: true}]) {
    const fixture = app(options);
    const result = refresh([fixture.value]);
    assert.match(result.error.message, /restart/);
    assert.equal(fixture.state.quits, 1);
    assert.deepEqual(result.events, []);
  }
});

test('multiple apps and failure to reopen report the required manual action', () => {
  const first = app(), second = app();
  const ambiguous = refresh([first.value, second.value]);
  assert.match(ambiguous.error.message, /Multiple Codex apps/);
  assert.equal(first.state.quits + second.state.quits, 0);
  assert.deepEqual(ambiguous.events, []);
  const failed = refresh([first.value], 1);
  assert.match(failed.error.message, /open it normally/);
  assert.equal(failed.events.length, 1);
});
