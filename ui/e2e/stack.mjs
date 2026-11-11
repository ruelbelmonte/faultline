// Starts faultline-demo, faultline proxy and faultline control for the UI
// smoke test, on ports the default configuration does not use.
import { spawn } from 'node:child_process'
import { mkdirSync, rmSync, writeFileSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const binDir = resolve(process.env.FAULTLINE_BIN_DIR ?? join(here, '..', '..', 'bin'))
const exe = process.platform === 'win32' ? '.exe' : ''
const work = join(here, '.work')

rmSync(work, { recursive: true, force: true })
mkdirSync(work, { recursive: true })

const config = join(work, 'faultline.yaml')
writeFileSync(
  config,
  `version: 1
environment: ci
data_dir: ${JSON.stringify(join(work, 'data'))}
experiments_dir: ${JSON.stringify(join(work, 'experiments'))}
proxy:
  listen: 127.0.0.1:17701
  admin_listen: 127.0.0.1:17702
control:
  listen: 127.0.0.1:17700
  proxy_admin_url: http://127.0.0.1:17702
routes:
  - name: inventory
    upstream: http://127.0.0.1:19001
  - name: pricing
    upstream: http://127.0.0.1:19002
  - name: llm
    upstream: http://127.0.0.1:19003
`,
)

// Synthetic tokens, used by this test stack only.
const env = {
  ...process.env,
  FAULTLINE_TOKEN: 'ui-smoke-control-token-0123',
  FAULTLINE_ADMIN_TOKEN: 'ui-smoke-admin-token-0123',
}

const children = [
  spawn(
    join(binDir, 'faultline-demo' + exe),
    ['--mode', 'naive', '--proxy', 'http://127.0.0.1:17701', '--shop', '127.0.0.1:19000', '--inventory', '127.0.0.1:19001', '--pricing', '127.0.0.1:19002', '--llm', '127.0.0.1:19003'],
    { env, stdio: 'inherit' },
  ),
  spawn(join(binDir, 'faultline' + exe), ['proxy', '--config', config], { env, stdio: 'inherit' }),
  spawn(join(binDir, 'faultline' + exe), ['control', '--config', config], { env, stdio: 'inherit' }),
]

let stopping = false
function stop(code) {
  if (stopping) return
  stopping = true
  for (const c of children) c.kill()
  process.exit(code)
}

for (const c of children) {
  c.on('error', (err) => {
    console.error(`could not start ${c.spawnfile}: ${err.message}. Run "make build" first.`)
    stop(1)
  })
  c.on('exit', (code) => {
    if (!stopping) {
      console.error(`${c.spawnfile} exited with code ${code}`)
      stop(1)
    }
  })
}
process.on('SIGINT', () => stop(0))
process.on('SIGTERM', () => stop(0))
