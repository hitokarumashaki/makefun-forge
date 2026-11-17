// Entry point for the make.fun github-core-clone-01 forge (v4: Go binary core).
// main.ts is a thin launcher: the server itself is the statically linked
// linux/amd64 Go binary committed beside it as ./forge. The binary is copied
// to a temp file and chmod +x before spawning, because a browser upload does
// not preserve the executable bit. The Go binary owns the whole lifecycle
// (bind an ephemeral port, register with the env, poll until done, exit 0);
// this process just waits for it and exits with its code.

import { spawn } from 'node:child_process';
import { copyFileSync, chmodSync, mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const dir = dirname(fileURLToPath(import.meta.url));
const src = join(dir, 'forge');
const tmp = mkdtempSync(join(tmpdir(), 'forge-'));
const bin = join(tmp, 'forge');
copyFileSync(src, bin);
chmodSync(bin, 0o755);

const child = spawn(bin, [], { stdio: 'inherit' });
child.on('error', (err: Error) => {
  process.stderr.write(`[entry] failed to start the Go binary: ${err.message}\n`);
  process.exit(1);
});
child.on('exit', (code: number | null, signal: NodeJS.Signals | null) => {
  rmSync(tmp, { recursive: true, force: true });
  if (signal) {
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code ?? 1);
});
