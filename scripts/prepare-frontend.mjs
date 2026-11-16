import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'

const frontend = fileURLToPath(new URL('../frontend/', import.meta.url))
const patch = fileURLToPath(new URL('frontend-naive-masquerade.patch', import.meta.url))
const options = { cwd: frontend }
const apply = ['apply', '--ignore-space-change']

// The checked-out submodule stays pinned to upstream. Apply our UI changes
// before compiling it, including when the source was copied without .git.
const check = spawnSync('git', [...apply, '--reverse', '--check', patch], options)
if (check.error) throw check.error
if (check.status !== 0) {
  const result = spawnSync('git', [...apply, patch], { ...options, stdio: 'inherit' })
  if (result.error) throw result.error
  if (result.status !== 0) process.exit(1)
}
