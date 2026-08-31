# QuorumLimiter

A three-node distributed token-bucket rate limiter whose state changes are ordered by a
Raft-lite consensus implementation written in Go.

> **Status:** early setup. This README is a placeholder and will be expanded during the
> documentation phase. See `QuorumLimiter.pdf` for the complete build guide.

## What it is

QuorumLimiter decides whether a caller may perform an action (for example, "may this user
send another password-reset email under a 5-per-minute policy?"). Every state-changing
decision is committed to a replicated log by a majority of nodes before it is applied and
returned, so three independent servers can never collectively over-allow.

## Planned stack

Go 1.26.5 · bbolt · Prometheus · Docker + Compose · Caddy

> **Note:** this is an educational Raft implementation with fixed three-node membership. It
> demonstrates consensus mechanics and failure handling; it is not a substitute for a
> formally verified production consensus library.
