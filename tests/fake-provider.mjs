#!/usr/bin/env node

import { spawn } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';

const provider = process.argv[2];
const providerArgs = process.argv.slice(3);
// A fan-out runs both providers from one process, so each needs its own
// scenario and session id to produce mixed outcomes without colliding locks.
const perProvider = (name, fallback) =>
  process.env[`${name}_${provider.toUpperCase()}`] ?? process.env[name] ?? fallback;
const scenario = perProvider('ENVOY_FAKE_SCENARIO', 'success');
const sessionId = perProvider('ENVOY_FAKE_SESSION_ID', 'fake-session-id');
const finalText = process.env.ENVOY_FAKE_FINAL_TEXT ?? 'fake provider result';
const invocationFile = perProvider('ENVOY_FAKE_INVOCATION_FILE', '');
if (invocationFile) {
  fs.writeFileSync(invocationFile, JSON.stringify({ argv: providerArgs, env: process.env, pid: process.pid }));
}

if (!['codex', 'claude'].includes(provider)) {
  process.stderr.write(`unknown fake provider: ${provider}\n`);
  process.exit(64);
}

if (scenario === 'exit-before-stdin') {
  process.stderr.write('fake provider exited before reading its prompt\n');
  process.exit(23);
}

// Match the real CLIs: consume the complete prompt and observe EOF before a
// turn starts. This catches regressions where turn.mjs forgets to close stdin.
let prompt = '';
for await (const chunk of process.stdin) prompt += chunk;
const promptFile = perProvider('ENVOY_FAKE_PROMPT_FILE', '');
if (promptFile) {
  fs.writeFileSync(promptFile, prompt);
}
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const emit = (event) => process.stdout.write(`${JSON.stringify(event)}\n`);

function claudeInit() {
  return {
    type: 'system',
    subtype: 'init',
    session_id: sessionId,
    model: 'fake-claude-model',
    claude_code_version: 'fake-2.1.207',
  };
}

function claudeAssistant(text) {
  return {
    type: 'assistant',
    session_id: sessionId,
    message: {
      role: 'assistant',
      content: [{ type: 'text', text }],
      usage: { input_tokens: 11, output_tokens: 7 },
    },
  };
}

function claudeResult(overrides = {}) {
  return {
    type: 'result',
    subtype: 'success',
    is_error: false,
    session_id: sessionId,
    result: finalText,
    total_cost_usd: 0.01,
    usage: {
      input_tokens: 11,
      cache_read_input_tokens: 4,
      cache_creation_input_tokens: 2,
      output_tokens: 7,
    },
    ...overrides,
  };
}

function outputPath() {
  const index = providerArgs.indexOf('-o');
  return index >= 0 ? providerArgs[index + 1] : undefined;
}

function claudeArgSessionId() {
  for (const flag of ['--session-id', '--resume']) {
    const index = providerArgs.indexOf(flag);
    if (index >= 0 && providerArgs[index + 1]) return providerArgs[index + 1];
  }
  return sessionId;
}

function emitCodexStart() {
  emit({ type: 'thread.started', thread_id: sessionId });
}

function emitCodexResult() {
  emit({ type: 'item.completed', item: { type: 'agent_message', text: finalText } });
  emit({
    type: 'turn.completed',
    usage: {
      input_tokens: 13,
      cached_input_tokens: 5,
      output_tokens: 8,
      reasoning_output_tokens: 3,
    },
  });
  const lastMessage = outputPath();
  if (lastMessage) {
    fs.mkdirSync(path.dirname(lastMessage), { recursive: true });
    fs.writeFileSync(lastMessage, finalText);
  }
}

function startGrandchild({ ignoreSigterm = false } = {}) {
  const readyFile = process.env.ENVOY_FAKE_GRANDCHILD_READY_FILE;
  const grandchildProgram = [
    ignoreSigterm ? "process.on('SIGTERM', () => {});" : '',
    readyFile ? `require('node:fs').writeFileSync(${JSON.stringify(readyFile)}, 'ready\\n');` : '',
    'setInterval(() => {}, 60_000);',
  ].join(' ');
  const grandchild = spawn(process.execPath, [
    '-e',
    grandchildProgram,
  ], { stdio: 'inherit' });
  grandchild.unref();
  if (!process.env.ENVOY_FAKE_GRANDCHILD_PID_FILE) {
    throw new Error('ENVOY_FAKE_GRANDCHILD_PID_FILE is required for a tree scenario');
  }
  fs.writeFileSync(process.env.ENVOY_FAKE_GRANDCHILD_PID_FILE, `${grandchild.pid}\n`);
  return grandchild;
}

if (provider === 'claude') {
  if (scenario === 'usage-capture') {
    const fixture = new URL('../internal/provider/testdata/claude-2.1.270-usage.jsonl', import.meta.url);
    const records = fs.readFileSync(fixture, 'utf8').trim().split('\n').map(JSON.parse);
    for (const [i, event] of records.slice(0, -1).entries()) {
      emit(event);
      // The test releases each gate only after reading the new live snapshot.
      // A long heartbeat prevents periodic writes from hiding a missing event write.
      if (process.env.ENVOY_FAKE_USAGE_GATE && (i === 1 || i === 3)) {
        const gate = `${process.env.ENVOY_FAKE_USAGE_GATE}-${i}`;
        const deadline = Date.now() + 10000;
        while (!fs.existsSync(gate)) {
          if (Date.now() > deadline) throw new Error('usage test gate timed out');
          await sleep(10);
        }
      }
    }
    const ending = process.env.ENVOY_FAKE_USAGE_END ?? 'success';
    const result = records.at(-1);
    result.session_id = claudeArgSessionId();
    if (ending === 'missing') delete result.usage;
    if (ending === 'budget') result.subtype = 'error_max_budget_usd';
    if (ending === 'crash') {
      result.subtype = 'error_during_execution';
      result.is_error = true;
      result.usage = { input_tokens: 0, cache_read_input_tokens: 0, cache_creation_input_tokens: 0, output_tokens: 0 };
    }
    if (ending === 'hang') await sleep(60000);
    if (ending !== 'eof') process.stdout.write(JSON.stringify(result));
  } else if (scenario === 'success') {
    emit(claudeInit());
    emit(claudeAssistant('claude is finishing the fake task'));
    process.stdout.write(JSON.stringify(claudeResult())); // no final newline exercises close-time flush
  } else if (scenario === 'delayed-success') {
    emit(claudeInit());
    await sleep(Number(process.env.ENVOY_FAKE_START_DELAY_MS ?? 50));
    emit(claudeAssistant('claude completed an intermediate step'));
    await sleep(Number(process.env.ENVOY_FAKE_DELAY_MS ?? 500));
    process.stdout.write(JSON.stringify(claudeResult()));
  } else if (scenario === 'fragmented-success') {
    emit(claudeInit());
    const assistant = Buffer.from(`${JSON.stringify(claudeAssistant('split UTF-8 🧭 event'))}\n`);
    const marker = Buffer.from('🧭');
    const markerAt = assistant.indexOf(marker);
    process.stdout.write(assistant.subarray(0, markerAt + 1));
    await sleep(20);
    process.stdout.write(assistant.subarray(markerAt + 1));
    const result = Buffer.from(JSON.stringify(claudeResult()));
    const splitAt = Math.floor(result.length / 2);
    process.stdout.write(result.subarray(0, splitAt));
    await sleep(20);
    process.stdout.write(result.subarray(splitAt));
  } else if (scenario === 'success-with-stubborn-grandchild') {
    emit(claudeInit());
    emit(claudeAssistant('claude completed before residual cleanup'));
    emit(claudeResult());
    startGrandchild({ ignoreSigterm: true });
    const readyFile = process.env.ENVOY_FAKE_GRANDCHILD_READY_FILE;
    if (readyFile) {
      while (!fs.existsSync(readyFile)) await sleep(5);
    }
  } else if (scenario === 'terminal-success-then-hang') {
    emit(claudeInit());
    emit(claudeAssistant('claude completed before its CLI stalled'));
    emit(claudeResult());
    setInterval(() => {}, 60_000);
  } else if (scenario === 'hang-with-stubborn-grandchild-only') {
    emit(claudeInit());
    emit(claudeAssistant('useful claude work before timeout'));
    startGrandchild({ ignoreSigterm: true });
    setInterval(() => {}, 60_000);
  } else if (scenario === 'init-only-hang') {
    emit(claudeInit());
    setInterval(() => {}, 60_000);
  } else if (scenario === 'transcript-only-hang') {
    const id = claudeArgSessionId();
    const transcriptDir = path.join(process.env.HOME, '.claude', 'projects', 'fake-project');
    fs.mkdirSync(transcriptDir, { recursive: true });
    const now = new Date().toISOString();
    const rows = [
      { type: 'user', timestamp: now, message: { role: 'user', content: [{ type: 'text', text: prompt }] } },
      { type: 'assistant', timestamp: now, message: { role: 'assistant', content: [{ type: 'text', text: 'partial work from the Claude transcript' }] } },
    ];
    fs.writeFileSync(path.join(transcriptDir, `${id}.jsonl`), `${rows.map((row) => JSON.stringify(row)).join('\n')}\n`);
    setInterval(() => {}, 60_000);
  } else if (scenario === 'partial-failure') {
    const reason = 'synthetic provider failure';
    emit(claudeInit());
    emit(claudeAssistant('useful claude work before failure'));
    emit(claudeAssistant(reason));
    process.stdout.write(JSON.stringify(claudeResult({
      subtype: 'error_during_execution',
      is_error: true,
      result: undefined,
      errors: [reason],
    })));
    process.exitCode = 7;
  } else {
    process.stderr.write(`unsupported fake claude scenario: ${scenario}\n`);
    process.exit(64);
  }
} else if (scenario === 'success') {
  emitCodexStart();
  emitCodexResult();
} else if (scenario === 'nonzero-final') {
  emitCodexStart();
  emitCodexResult();
  process.exitCode = 7;
} else if (scenario === 'reconnect-then-success') {
  // The link dropped and came back: codex-cli emits one bare `error` per
  // reconnect attempt and then finishes the turn normally.
  emitCodexStart();
  emit({ type: 'error', message: 'Reconnecting... 1/5 (stream disconnected before completion: failed to lookup address information: nodename nor servname provided, or not known)' });
  emit({ type: 'error', message: 'Reconnecting... waiting for network (Connection failed: error sending request)' });
  emit({ type: 'error', message: 'something unrelated and transient' });
  emitCodexResult();
} else if (scenario === 'delayed-success') {
  await sleep(Number(process.env.ENVOY_FAKE_START_DELAY_MS ?? 0));
  emitCodexStart();
  await sleep(Number(process.env.ENVOY_FAKE_DELAY_MS ?? 500));
  emitCodexResult();
} else if (scenario === 'success-with-stubborn-grandchild') {
  emitCodexStart();
  emitCodexResult();
  startGrandchild({ ignoreSigterm: true });
  const readyFile = process.env.ENVOY_FAKE_GRANDCHILD_READY_FILE;
  if (readyFile) {
    while (!fs.existsSync(readyFile)) await sleep(5);
  }
} else if (scenario === 'terminal-success-then-hang') {
  emitCodexStart();
  emitCodexResult();
  setInterval(() => {}, 60_000);
} else if (scenario === 'stderr-noise-then-silence') {
  // The real shape of a stalled codex turn on a machine whose models cache is
  // corrupt: a diagnostic on stderr, then no parseable event, ever. Bytes
  // arrived; nothing envoy can read from them did.
  process.stderr.write('ERROR provider_models_cache: failed to load models cache\n');
  setInterval(() => {}, 60_000);
} else if (scenario === 'quiet-then-emits-on-term') {
  // Accepts, goes silent until the cap, then streams one more event while
  // being torn down — the race that decides whether a capped turn's reported
  // stream describes the moment the cap arrived or the moment cleanup ended.
  emitCodexStart();
  process.on('SIGTERM', () => {
    emit({ type: 'item.updated' });
    setTimeout(() => process.exit(0), 40);
  });
  setInterval(() => {}, 60_000);
} else if (scenario === 'hang-with-stubborn-grandchild-only') {
  emitCodexStart();
  startGrandchild({ ignoreSigterm: true });
  setInterval(() => {}, 60_000);
} else {
  process.stderr.write(`unsupported fake codex scenario: ${scenario}\n`);
  process.exit(64);
}
