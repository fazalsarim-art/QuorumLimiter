package limiter

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/fazalsarim-art/QuorumLimiter/internal/storage"
)

// Reject codes are stable, non-secret identifiers for business rejections. HTTP
// status mapping happens at the API layer.
const (
	RejectValidation          = "validation_failed"
	RejectPolicyNotFound      = "policy_not_found"
	RejectPolicyInactive      = "policy_inactive"
	RejectPolicyExists        = "policy_exists"
	RejectVersionConflict     = "version_conflict"
	RejectClientNotFound      = "client_not_found"
	RejectClientRevoked       = "client_revoked"
	RejectForbiddenPolicy     = "forbidden_policy"
	RejectKeyPrefixConflict   = "key_prefix_conflict"
	RejectClientExists        = "client_exists"
	RejectIdempotencyConflict = "idempotency_conflict"
)

// idempotencyTTLms is how long a decision record is retained for replay.
const idempotencyTTLms int64 = 24 * 60 * 60 * 1000

// defaultAuditKeepMax bounds retained audit records when a prune command does
// not specify a limit.
const defaultAuditKeepMax = 100_000

// Outcome is the high-level category of an apply result.
type Outcome string

const (
	OutcomeAllowed  Outcome = "allowed"
	OutcomeDenied   Outcome = "denied"
	OutcomeApplied  Outcome = "applied"
	OutcomeRejected Outcome = "rejected"
)

// Result is what applying a command produces for the waiting caller. Business
// rejections are conveyed here (not as Go errors) so that last_applied still
// advances and the entry is not retried forever.
type Result struct {
	Type       CommandType
	Outcome    Outcome
	Duplicate  bool
	Decision   *DecisionRecord
	Policy     *Policy
	Client     *Client
	Prune      *PruneResult
	RejectCode string
	RejectMsg  string
}

// PruneResult reports how many records a prune command removed.
type PruneResult struct {
	IdempotencyRemoved int
	AuditRemoved       int
}

// ApplyContext carries the Raft-provided facts about the committed entry.
type ApplyContext struct {
	LogIndex uint64
	Term     uint64
}

// StateMachine applies committed commands to the durable application state. It
// holds no mutable state of its own; determinism comes from the command inputs.
type StateMachine struct{}

// New returns a StateMachine.
func New() *StateMachine { return &StateMachine{} }

// ApplyEntry applies one committed Raft log entry. It is the bridge between the
// consensus layer and the state machine: a command entry is decoded and applied
// (advancing last_applied atomically with its effects), while a no-op or
// sentinel entry has no application effect and only advances the applied
// checkpoint. In all cases last_applied advances so the apply loop makes
// progress.
func (sm *StateMachine) ApplyEntry(store *storage.Store, index, term uint64, entry storage.LogEntry) (Result, error) {
	if entry.Kind != storage.KindCommand {
		if err := store.SetLastApplied(index); err != nil {
			return Result{}, err
		}
		return Result{Outcome: OutcomeApplied}, nil
	}
	cmd, err := DecodeCommand(entry.Command)
	if err != nil {
		return Result{}, err
	}
	return sm.Apply(store, ApplyContext{LogIndex: index, Term: term}, cmd)
}

// Apply applies one committed command inside a single storage transaction that
// also advances last_applied, so a crash leaves either all or none of the
// entry's effects. Only infrastructure errors are returned; business rejections
// are reported in Result and still advance last_applied.
func (sm *StateMachine) Apply(store *storage.Store, ctx ApplyContext, cmd Command) (Result, error) {
	var res Result
	err := store.Apply(func(tx *storage.StateTx) error {
		r, aerr := sm.applyTx(tx, ctx, cmd)
		if aerr != nil {
			return aerr
		}
		res = r
		return tx.SetLastApplied(ctx.LogIndex)
	})
	if err != nil {
		return Result{}, err
	}
	return res, nil
}

func reject(typ CommandType, code, msg string) (Result, error) {
	return Result{Type: typ, Outcome: OutcomeRejected, RejectCode: code, RejectMsg: msg}, nil
}

func (sm *StateMachine) applyTx(tx *storage.StateTx, ctx ApplyContext, cmd Command) (Result, error) {
	switch cmd.Type {
	case CmdCreatePolicy:
		return sm.applyCreatePolicy(tx, ctx, cmd)
	case CmdUpdatePolicy:
		return sm.applyUpdatePolicy(tx, ctx, cmd)
	case CmdSetPolicyActive:
		return sm.applySetPolicyActive(tx, ctx, cmd)
	case CmdCreateClient:
		return sm.applyCreateClient(tx, ctx, cmd)
	case CmdRevokeClient:
		return sm.applyRevokeClient(tx, ctx, cmd)
	case CmdDecide:
		return sm.applyDecide(tx, ctx, cmd)
	case CmdPrune:
		return sm.applyPrune(tx, cmd)
	default:
		// An unknown command type in a committed entry indicates a corrupt log
		// or a version mismatch; this is not a business rejection.
		return Result{}, fmt.Errorf("limiter: unknown command type %q", cmd.Type)
	}
}

// --- policy commands ---

func (sm *StateMachine) applyCreatePolicy(tx *storage.StateTx, ctx ApplyContext, cmd Command) (Result, error) {
	var p CreatePolicyPayload
	if err := unmarshalPayload(cmd.Payload, &p); err != nil {
		return Result{}, err
	}
	if err := validatePolicyID(p.ID); err != nil {
		return reject(CmdCreatePolicy, RejectValidation, err.Error())
	}
	name, err := validateName(p.Name, maxPolicyName)
	if err != nil {
		return reject(CmdCreatePolicy, RejectValidation, err.Error())
	}
	if err := validatePolicyFields(p.CapacityTokens, p.RefillTokens, p.RefillIntervalMS, p.MaxCostTokens); err != nil {
		return reject(CmdCreatePolicy, RejectValidation, err.Error())
	}
	if _, exists := tx.GetPolicy(p.ID); exists {
		return reject(CmdCreatePolicy, RejectPolicyExists, "policy id already exists")
	}

	policy := Policy{
		SchemaVersion:    ModelSchemaVersion,
		ID:               p.ID,
		Name:             name,
		CapacityTokens:   p.CapacityTokens,
		RefillTokens:     p.RefillTokens,
		RefillIntervalMS: p.RefillIntervalMS,
		MaxCostTokens:    p.MaxCostTokens,
		Active:           p.Active,
		Version:          1,
		CreatedAtMS:      cmd.TimestampMS,
		UpdatedAtMS:      cmd.TimestampMS,
	}
	if err := putPolicy(tx, policy); err != nil {
		return Result{}, err
	}
	if err := writeAdminAudit(tx, ctx, "policy_created", policy.ID, cmd.TimestampMS); err != nil {
		return Result{}, err
	}
	return Result{Type: CmdCreatePolicy, Outcome: OutcomeApplied, Policy: &policy}, nil
}

func (sm *StateMachine) applyUpdatePolicy(tx *storage.StateTx, ctx ApplyContext, cmd Command) (Result, error) {
	var p UpdatePolicyPayload
	if err := unmarshalPayload(cmd.Payload, &p); err != nil {
		return Result{}, err
	}
	raw, ok := tx.GetPolicy(p.ID)
	if !ok {
		return reject(CmdUpdatePolicy, RejectPolicyNotFound, "policy does not exist")
	}
	old, err := decodePolicy(raw)
	if err != nil {
		return Result{}, err
	}
	if old.Version != p.ExpectedVersion {
		return reject(CmdUpdatePolicy, RejectVersionConflict, "policy version has changed")
	}
	name, verr := validateName(p.Name, maxPolicyName)
	if verr != nil {
		return reject(CmdUpdatePolicy, RejectValidation, verr.Error())
	}
	if verr := validatePolicyFields(p.CapacityTokens, p.RefillTokens, p.RefillIntervalMS, p.MaxCostTokens); verr != nil {
		return reject(CmdUpdatePolicy, RejectValidation, verr.Error())
	}

	newPolicy := old
	newPolicy.Name = name
	newPolicy.CapacityTokens = p.CapacityTokens
	newPolicy.RefillTokens = p.RefillTokens
	newPolicy.RefillIntervalMS = p.RefillIntervalMS
	newPolicy.MaxCostTokens = p.MaxCostTokens
	newPolicy.Version = old.Version + 1
	newPolicy.UpdatedAtMS = cmd.TimestampMS

	// Deterministically convert every existing bucket for this policy: refill it
	// under the OLD policy through the update timestamp, clamp to the NEW
	// capacity, reset the remainder if the interval changed, and stamp the new
	// version. This prevents the new refill rate from being applied
	// retroactively to already-elapsed time.
	intervalChanged := old.RefillIntervalMS != p.RefillIntervalMS
	newCapMilli := p.CapacityTokens * milliPerToken

	var toWrite []TokenBucketState
	err = tx.ForEachTokenBucketByPolicy(p.ID, func(_ string, val []byte) error {
		b, derr := decodeBucket(val)
		if derr != nil {
			return derr
		}
		refillBucket(&b, old.CapacityTokens, old.RefillTokens, old.RefillIntervalMS, cmd.TimestampMS)
		if b.TokensMilli > newCapMilli {
			b.TokensMilli = newCapMilli
		}
		if intervalChanged {
			b.RefillRemainder = 0
		}
		b.PolicyVersion = newPolicy.Version
		toWrite = append(toWrite, b)
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	for _, b := range toWrite {
		enc, encErr := encodeBucket(b)
		if encErr != nil {
			return Result{}, encErr
		}
		if perr := tx.PutTokenBucket(b.PolicyID, b.Subject, enc); perr != nil {
			return Result{}, perr
		}
	}

	if err := putPolicy(tx, newPolicy); err != nil {
		return Result{}, err
	}
	if err := writeAdminAudit(tx, ctx, "policy_updated", newPolicy.ID, cmd.TimestampMS); err != nil {
		return Result{}, err
	}
	return Result{Type: CmdUpdatePolicy, Outcome: OutcomeApplied, Policy: &newPolicy}, nil
}

func (sm *StateMachine) applySetPolicyActive(tx *storage.StateTx, ctx ApplyContext, cmd Command) (Result, error) {
	var p SetPolicyActivePayload
	if err := unmarshalPayload(cmd.Payload, &p); err != nil {
		return Result{}, err
	}
	raw, ok := tx.GetPolicy(p.ID)
	if !ok {
		return reject(CmdSetPolicyActive, RejectPolicyNotFound, "policy does not exist")
	}
	policy, err := decodePolicy(raw)
	if err != nil {
		return Result{}, err
	}
	if policy.Version != p.ExpectedVersion {
		return reject(CmdSetPolicyActive, RejectVersionConflict, "policy version has changed")
	}
	policy.Active = p.Active
	policy.Version++
	policy.UpdatedAtMS = cmd.TimestampMS
	if err := putPolicy(tx, policy); err != nil {
		return Result{}, err
	}
	event := "policy_deactivated"
	if p.Active {
		event = "policy_activated"
	}
	if err := writeAdminAudit(tx, ctx, event, policy.ID, cmd.TimestampMS); err != nil {
		return Result{}, err
	}
	return Result{Type: CmdSetPolicyActive, Outcome: OutcomeApplied, Policy: &policy}, nil
}

// --- client commands ---

func (sm *StateMachine) applyCreateClient(tx *storage.StateTx, ctx ApplyContext, cmd Command) (Result, error) {
	var p CreateClientPayload
	if err := unmarshalPayload(cmd.Payload, &p); err != nil {
		return Result{}, err
	}
	name, verr := validateName(p.Name, maxClientName)
	if verr != nil {
		return reject(CmdCreateClient, RejectValidation, verr.Error())
	}
	if verr := validateAllowedPolicies(p.AllowedPolicyIDs); verr != nil {
		return reject(CmdCreateClient, RejectValidation, verr.Error())
	}
	if p.ID == "" || p.KeyPrefix == "" || len(p.KeyDigest) == 0 {
		return reject(CmdCreateClient, RejectValidation, "client id, key prefix, and key digest are required")
	}
	if _, exists := tx.GetClient(p.ID); exists {
		return reject(CmdCreateClient, RejectClientExists, "client id already exists")
	}
	if _, exists := tx.GetClientIDByPrefix(p.KeyPrefix); exists {
		return reject(CmdCreateClient, RejectKeyPrefixConflict, "key prefix already in use")
	}
	// Referenced policies must exist unless the client is granted all ("*").
	if len(p.AllowedPolicyIDs) != 1 || p.AllowedPolicyIDs[0] != "*" {
		for _, id := range p.AllowedPolicyIDs {
			if _, ok := tx.GetPolicy(id); !ok {
				return reject(CmdCreateClient, RejectPolicyNotFound, "allowed policy "+id+" does not exist")
			}
		}
	}

	client := Client{
		SchemaVersion:    ModelSchemaVersion,
		ID:               p.ID,
		Name:             name,
		KeyPrefix:        p.KeyPrefix,
		KeyDigest:        p.KeyDigest,
		AllowedPolicyIDs: p.AllowedPolicyIDs,
		Active:           true,
		CreatedAtMS:      cmd.TimestampMS,
		RevokedAtMS:      nil,
	}
	if err := putClient(tx, client); err != nil {
		return Result{}, err
	}
	if err := tx.PutClientPrefix(client.KeyPrefix, client.ID); err != nil {
		return Result{}, err
	}
	if err := writeAdminAudit(tx, ctx, "client_created", "", cmd.TimestampMS); err != nil {
		return Result{}, err
	}
	return Result{Type: CmdCreateClient, Outcome: OutcomeApplied, Client: &client}, nil
}

func (sm *StateMachine) applyRevokeClient(tx *storage.StateTx, ctx ApplyContext, cmd Command) (Result, error) {
	var p RevokeClientPayload
	if err := unmarshalPayload(cmd.Payload, &p); err != nil {
		return Result{}, err
	}
	raw, ok := tx.GetClient(p.ID)
	if !ok {
		return reject(CmdRevokeClient, RejectClientNotFound, "client does not exist")
	}
	client, err := decodeClient(raw)
	if err != nil {
		return Result{}, err
	}
	if !client.Active {
		// Already revoked: revocation is idempotent, so report success and flag
		// it as a duplicate so the API can return already_revoked.
		return Result{Type: CmdRevokeClient, Outcome: OutcomeApplied, Client: &client, Duplicate: true}, nil
	}
	client.Active = false
	ts := cmd.TimestampMS
	client.RevokedAtMS = &ts
	if err := putClient(tx, client); err != nil {
		return Result{}, err
	}
	if err := writeAdminAudit(tx, ctx, "client_revoked", "", cmd.TimestampMS); err != nil {
		return Result{}, err
	}
	return Result{Type: CmdRevokeClient, Outcome: OutcomeApplied, Client: &client}, nil
}

// --- decision command ---

func (sm *StateMachine) applyDecide(tx *storage.StateTx, ctx ApplyContext, cmd Command) (Result, error) {
	var p DecidePayload
	if err := unmarshalPayload(cmd.Payload, &p); err != nil {
		return Result{}, err
	}

	// Validate request shape before any state change.
	if err := validatePolicyID(p.PolicyID); err != nil {
		return reject(CmdDecide, RejectValidation, err.Error())
	}
	if err := validateSubject(p.Subject); err != nil {
		return reject(CmdDecide, RejectValidation, err.Error())
	}
	if err := validateRequestID(p.RequestID); err != nil {
		return reject(CmdDecide, RejectValidation, err.Error())
	}
	if p.Cost < 1 {
		return reject(CmdDecide, RejectValidation, "cost must be at least 1")
	}

	digest := requestDigest(p.PolicyID, p.Subject, p.Cost)

	// Idempotency lookup BEFORE any refill or deduction.
	if raw, ok, err := tx.GetIdempotency(p.ClientID, p.RequestID); err != nil {
		return Result{}, err
	} else if ok {
		prior, derr := decodeDecision(raw)
		if derr != nil {
			return Result{}, derr
		}
		if prior.RequestDigest != digest {
			return reject(CmdDecide, RejectIdempotencyConflict, "idempotency key reused with different request content")
		}
		out := OutcomeAllowed
		if !prior.Allowed {
			out = OutcomeDenied
		}
		return Result{Type: CmdDecide, Outcome: out, Duplicate: true, Decision: &prior}, nil
	}

	// Client authorization.
	craw, ok := tx.GetClient(p.ClientID)
	if !ok {
		return reject(CmdDecide, RejectClientNotFound, "unknown client")
	}
	client, err := decodeClient(craw)
	if err != nil {
		return Result{}, err
	}
	if !client.Active {
		return reject(CmdDecide, RejectClientRevoked, "client is revoked")
	}
	if !client.AllowsPolicy(p.PolicyID) {
		return reject(CmdDecide, RejectForbiddenPolicy, "client may not use this policy")
	}

	// Policy lookup.
	praw, ok := tx.GetPolicy(p.PolicyID)
	if !ok {
		return reject(CmdDecide, RejectPolicyNotFound, "unknown policy")
	}
	policy, err := decodePolicy(praw)
	if err != nil {
		return Result{}, err
	}
	if !policy.Active {
		return reject(CmdDecide, RejectPolicyInactive, "policy is inactive")
	}
	if p.Cost > policy.MaxCostTokens {
		return reject(CmdDecide, RejectValidation, "cost exceeds policy maximum")
	}

	// Load or initialize the bucket (a new subject starts full).
	bucket, err := loadOrInitBucket(tx, policy, p.Subject, cmd.TimestampMS)
	if err != nil {
		return Result{}, err
	}

	refillBucket(&bucket, policy.CapacityTokens, policy.RefillTokens, policy.RefillIntervalMS, cmd.TimestampMS)
	allowed, retryAfter := spend(&bucket, p.Cost, policy.RefillTokens, policy.RefillIntervalMS)
	bucket.PolicyVersion = policy.Version
	bucket.UpdatedLogIndex = ctx.LogIndex

	if err := putBucket(tx, bucket); err != nil {
		return Result{}, err
	}

	record := DecisionRecord{
		SchemaVersion:  ModelSchemaVersion,
		RequestID:      p.RequestID,
		RequestDigest:  digest,
		ClientID:       p.ClientID,
		PolicyID:       p.PolicyID,
		Subject:        p.Subject,
		CostTokens:     p.Cost,
		Allowed:        allowed,
		RemainingMilli: bucket.TokensMilli,
		RetryAfterMS:   retryAfter,
		ObservedAtMS:   cmd.TimestampMS,
		ExpiresAtMS:    cmd.TimestampMS + idempotencyTTLms,
		LeaderTerm:     ctx.Term,
		LogIndex:       ctx.LogIndex,
	}
	enc, err := encodeDecision(record)
	if err != nil {
		return Result{}, err
	}
	if err := tx.PutIdempotency(p.ClientID, p.RequestID, enc); err != nil {
		return Result{}, err
	}

	actorType := p.ActorType
	if actorType == "" {
		actorType = "client"
	}
	audit := AuditEvent{
		SchemaVersion:  ModelSchemaVersion,
		EventType:      "decision",
		TimestampMS:    cmd.TimestampMS,
		ActorType:      actorType,
		ActorID:        p.ClientID,
		RequestID:      p.RequestID,
		PolicyID:       p.PolicyID,
		SubjectHash:    subjectHash(p.Subject),
		Allowed:        allowed,
		CostTokens:     p.Cost,
		RemainingMilli: bucket.TokensMilli,
		Term:           ctx.Term,
		LogIndex:       ctx.LogIndex,
	}
	aenc, err := encodeAudit(audit)
	if err != nil {
		return Result{}, err
	}
	if err := tx.PutAudit(ctx.LogIndex, aenc); err != nil {
		return Result{}, err
	}

	out := OutcomeAllowed
	if !allowed {
		out = OutcomeDenied
	}
	return Result{Type: CmdDecide, Outcome: out, Decision: &record}, nil
}

// --- prune command ---

func (sm *StateMachine) applyPrune(tx *storage.StateTx, cmd Command) (Result, error) {
	var p PrunePayload
	if err := unmarshalPayload(cmd.Payload, &p); err != nil {
		return Result{}, err
	}
	keep := p.AuditKeepMax
	if keep <= 0 {
		keep = defaultAuditKeepMax
	}
	cutoff := cmd.TimestampMS

	// Expired idempotency records (collect keys, then delete).
	var expiredKeys [][]byte
	err := tx.ForEachIdempotency(func(key, val []byte) error {
		rec, derr := decodeDecision(val)
		if derr != nil {
			return derr
		}
		if rec.ExpiresAtMS <= cutoff {
			expiredKeys = append(expiredKeys, key)
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	for _, k := range expiredKeys {
		if derr := tx.DeleteIdempotency(k); derr != nil {
			return Result{}, derr
		}
	}

	// Trim audits to the newest keep records (audit keys ascend by log index).
	var auditIndexes []uint64
	err = tx.ForEachAudit(func(index uint64, _ []byte) error {
		auditIndexes = append(auditIndexes, index)
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	auditRemoved := 0
	if len(auditIndexes) > keep {
		remove := len(auditIndexes) - keep
		for i := 0; i < remove; i++ {
			if derr := tx.DeleteAudit(auditIndexes[i]); derr != nil {
				return Result{}, derr
			}
		}
		auditRemoved = remove
	}

	return Result{
		Type:    CmdPrune,
		Outcome: OutcomeApplied,
		Prune:   &PruneResult{IdempotencyRemoved: len(expiredKeys), AuditRemoved: auditRemoved},
	}, nil
}

// --- helpers ---

func loadOrInitBucket(tx *storage.StateTx, policy Policy, subject string, nowMS int64) (TokenBucketState, error) {
	raw, ok, err := tx.GetTokenBucket(policy.ID, subject)
	if err != nil {
		return TokenBucketState{}, err
	}
	if ok {
		return decodeBucket(raw)
	}
	// A new subject starts at full capacity as of the command timestamp.
	return TokenBucketState{
		SchemaVersion:   ModelSchemaVersion,
		PolicyID:        policy.ID,
		Subject:         subject,
		TokensMilli:     policy.CapacityTokens * milliPerToken,
		LastRefillMS:    nowMS,
		RefillRemainder: 0,
		PolicyVersion:   policy.Version,
	}, nil
}

// writeAdminAudit records an admin mutation, keyed by the committed log index.
// It never stores secrets (no keys, subjects, or tokens).
func writeAdminAudit(tx *storage.StateTx, ctx ApplyContext, eventType, policyID string, ts int64) error {
	audit := AuditEvent{
		SchemaVersion: ModelSchemaVersion,
		EventType:     eventType,
		TimestampMS:   ts,
		ActorType:     "admin",
		ActorID:       "admin",
		PolicyID:      policyID,
		Term:          ctx.Term,
		LogIndex:      ctx.LogIndex,
	}
	enc, err := encodeAudit(audit)
	if err != nil {
		return err
	}
	return tx.PutAudit(ctx.LogIndex, enc)
}

func putPolicy(tx *storage.StateTx, p Policy) error {
	enc, err := encodePolicy(p)
	if err != nil {
		return err
	}
	return tx.PutPolicy(p.ID, enc)
}

func putClient(tx *storage.StateTx, c Client) error {
	enc, err := encodeClient(c)
	if err != nil {
		return err
	}
	return tx.PutClient(c.ID, enc)
}

func putBucket(tx *storage.StateTx, b TokenBucketState) error {
	enc, err := encodeBucket(b)
	if err != nil {
		return err
	}
	return tx.PutTokenBucket(b.PolicyID, b.Subject, enc)
}

// requestDigest is a deterministic fingerprint of the request content, used to
// detect an idempotency key reused with different content.
func requestDigest(policyID, subject string, cost int64) string {
	var sb strings.Builder
	sb.WriteString(policyID)
	sb.WriteByte('\n')
	sb.WriteString(subject)
	sb.WriteByte('\n')
	sb.WriteString(strconv.FormatInt(cost, 10))
	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:])
}

// subjectHash is a short, one-way hash used to redact subjects in audit records.
func subjectHash(subject string) string {
	sum := sha256.Sum256([]byte(subject))
	return hex.EncodeToString(sum[:8])
}
