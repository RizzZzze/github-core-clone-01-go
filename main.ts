// Entry point for the github-core-clone-01 forge.
//
// This file is a pure launcher: the forge itself is a statically linked Go
// binary (`forge`, committed in this repository) that implements the whole
// GitHub-compatible surface — git smart HTTP, REST v3, GraphQL and MCP — on a
// single-threaded epoll event loop. The launcher only resolves the binary
// next to this file, forwards the environment unchanged, and propagates the
// child's exit code, exactly like the platform's own starter does for its
// lifecycle (bind ephemeral port, register with MAKEFUN_ENV_URL, long-poll
// done, exit 0).
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const envUrl = process.env.MAKEFUN_ENV_URL;
if (envUrl === undefined || envUrl === '') {
  console.error('MAKEFUN_ENV_URL is not set');
  process.exit(1);
}

const binaryPath = path.join(path.dirname(fileURLToPath(import.meta.url)), 'forge');

const child = spawn(binaryPath, [], {
  stdio: 'inherit',
  env: process.env,
});

child.on('error', (error) => {
  console.error(`failed to start forge binary: ${error.message}`);
  process.exit(1);
});

for (const signal of ['SIGINT', 'SIGTERM'] as const) {
  process.on(signal, () => {
    child.kill(signal);
  });
}

child.on('exit', (code, signal) => {
  if (signal !== null) {
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code ?? 1);
});
