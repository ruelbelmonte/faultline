# Running Faultline in a shared QA environment

Faultline is built for local development, CI and shared QA environments. It is not built for production and refuses to start when `environment` is `prod`, `production` or `live`.

> Use synthetic data only. Do not deploy Faultline into an environment that holds real personal, health or financial data, even a "QA" one. The environment guard checks a name in a config file; it cannot know what data is behind the services. Request bodies pass through the proxy in memory, and although Faultline never stores bodies or sensitive header values, deciding that an environment is safe to experiment in remains your responsibility.

## Topology

Run the proxy and the control plane as two separate workloads (two containers, two services). They must not share a process: the proxy is on the data path and has to survive, and clean up after, a control plane crash.

```
 engineers, CI --TLS--> [ TLS terminating proxy ] --> control :7700
                                                         |
                                                         | admin API, private network only
                                                         v
 services under test ---------------------------------> proxy :7701 (traffic), :7702 (admin)
                                                         |
                                                         v
                                                   upstream dependencies
```

Both default to `127.0.0.1`. In containers set the listeners explicitly:

```yaml
environment: qa
proxy:
  listen: 0.0.0.0:7701
  admin_listen: 0.0.0.0:7702
control:
  listen: 0.0.0.0:7700
  proxy_admin_url: http://faultline-proxy.internal:7702
```

Both workloads read the same `faultline.yaml`.

## Exposing the ports safely

| Port | Who needs it | How to expose it |
|---|---|---|
| 7701 proxy traffic | the services under test | Private network only. It has no authentication: anyone who can reach it can call your upstreams through it. Restrict it with a network policy or security group to the services that are configured to use it. |
| 7702 proxy admin | the control plane only | Private network only, restricted to the control plane's address. Never publish it. It can install faults for all traffic. |
| 7700 control API and dashboard | engineers and CI | Behind a TLS terminating reverse proxy or load balancer, restricted to your VPN or office network. Faultline speaks plain HTTP; without TLS in front, the bearer token crosses the network in clear text. The CLI warns when it is about to do that. |

Faultline does not terminate TLS itself. Calls from the proxy to an `https://` upstream are verified as usual.

## Tokens

- `FAULTLINE_TOKEN` authenticates the control API and the dashboard. `FAULTLINE_ADMIN_TOKEN` authenticates the proxy admin API and is needed by both workloads.
- Use two different random values of at least 32 bytes (`openssl rand -hex 32`). Both processes refuse to start with a missing token or one shorter than 16 characters.
- Inject them from your secret store as environment variables. They never go in `faultline.yaml`, and Faultline never logs them.
- There is one control token for everyone: no user accounts, no roles. The audit log records what was done, not who did it. Treat the token like a shared admin password and rotate it by restarting both workloads with new values.

## Keeping other people's traffic safe

- Leave `control.allow_unscoped: false`. Experiments with `scope: all` are then rejected, and every fault applies only to requests that carry the run id in `X-Chaos-Run`.
- Requests from other users, other test suites and other run ids pass through untouched.
- Services between the entry point and the proxy have to forward `X-Chaos-Run` and `X-Chaos-Correlation` on their outgoing calls. A service that drops them does not get faults behind it; the run then ends `inconclusive` with `fault_not_triggered`.
- Only one run can be active at a time. A second `faultline run` gets a 409 until the first finishes.

## What happens when things go wrong

| Situation | Outcome |
|---|---|
| The control plane crashes or is killed mid-run | The proxy keeps serving. Faults stop at their `end_at` at the latest, and all of them are cleared `lease_timeout` after the last renewal (10s by default). On restart, control marks the run `aborted` (`control_restarted`). |
| The control plane is stopped gracefully | It clears the faults before exiting. |
| The proxy restarts | It starts with no faults. The run aborts with `lease_lost`. |
| Someone needs everything to stop now | `faultline stop`, the dashboard's Kill switch, or `POST /api/v1/kill`. New requests are unaffected within a second and held connections are released. |
| The system under test degrades too far | Set `abort.error_rate_max` and `abort.probe_failures_consecutive` in the experiment; the run aborts itself. |

Keep the clocks of the proxy and control hosts in sync (NTP). Fault windows are absolute times set by control and enforced by the proxy.

## Data that Faultline keeps

Under `data_dir`, on the control plane's volume:

- `faultline.db`: runs, per-second metrics, audit entries, and the redacted copy of each experiment.
- `audit.jsonl`: the audit log, append-only from the application's side. It is a plain file; protect it with file permissions and ship it to your log store if you need it to be tamper-evident.
- `reports/{run_id}/`: the four report files.

Not kept: request or response bodies, header values, query strings. Values of headers listed in `redaction.headers` are replaced by `[REDACTED]` in anything stored or shown. The data directory is created with owner-only permissions. Give it a persistent volume if run history matters, and delete it to forget everything.

Logs are structured (`--log-format json`). They contain run ids, route names, statuses and error messages from failed upstream calls, which can include upstream host names.

## Checklist

- [ ] The environment holds synthetic data only.
- [ ] `environment: qa`, `control.allow_unscoped: false`.
- [ ] Proxy and control run as separate workloads from the same config.
- [ ] 7702 is reachable from the control plane only; 7701 from the services under test only.
- [ ] 7700 is behind TLS and network restrictions.
- [ ] Both tokens come from the secret store, are different, and are at least 32 random bytes.
- [ ] Services forward `X-Chaos-Run` and `X-Chaos-Correlation`.
- [ ] Experiments have `abort` thresholds, and the team knows where the kill switch is.
