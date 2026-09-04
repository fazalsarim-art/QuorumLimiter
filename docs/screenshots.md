# Screenshot checklist

These screenshots make the project legible to a reviewer in the first minute.
They are **manual** — capture them from your own running cluster and place them
under `docs/assets/`. Inspect every pixel before committing.

## Never capture (redact or retake)

A screenshot that contains any of the following must be retaken or edited before
it is committed — treat this as a hard rule:

- **Raw API keys** (`qlk_...`) — blur the secret portion; only the prefix may show.
- **Admin tokens or session/CSRF cookies** — never visible, including in devtools.
- **Passwords or password-manager popups.**
- **Real user data** — use synthetic subjects such as `demo_user_0042`.
- **Terminal prompts / shell history** — crop them out.
- **Private IPs or sensitive domains** — crop or blur if not meant to be public.
- **Browser extensions, bookmarks, and unrelated tabs** — crop them out.

Capture at **1440×900** (or a similarly readable size).

## Shots to capture

| File | Image | What it should prove | What to hide |
|------|-------|----------------------|--------------|
| `docs/assets/overview.png` | Overview | One leader, three healthy nodes, request summary | Public IPs / sensitive domains |
| `docs/assets/policies.png` | Policies | A concrete token-bucket policy configuration | Nothing secret should appear |
| `docs/assets/new-client.png` | New client confirmation | One-time raw-key behavior | **Blur the complete raw key** |
| `docs/assets/decisions.png` | Decisions | Allowed and denied audit rows | Full subjects and client identifiers |
| `docs/assets/cluster-before.png` | Cluster before failure | Matching commit and apply indexes | Private addresses (optional) |
| `docs/assets/cluster-after.png` | Cluster after failure | New leader and a higher term | No tokens or cookies |
| `docs/assets/prometheus.png` | Prometheus | Election and decision metrics | Query history with sensitive labels |
| `docs/assets/ci.png` | CI | Passing tests and security gates | No repository secrets ever visible |
| `docs/assets/failover.gif` | Failover (optional GIF) | Leader failover and continued serving | Terminal history, secrets |

## After capturing

- Reference the images from `README.md` (the demonstration section) and the docs.
- Re-verify that no secret, token, cookie, or real identifier is visible at full
  zoom before `git add`.
- If a secret ever reaches a committed image, **rotate it** and rewrite history
  before publishing — deleting the current file is not enough.
