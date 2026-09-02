package limiter

import (
	"fmt"
	"regexp"
	"strings"
)

// Field bounds, matching the documented data models.
const (
	minName            = 3
	maxPolicyName      = 64
	maxClientName      = 80
	minCapacity        = 1
	maxCapacity        = 1_000_000
	minRefill          = 1
	maxRefill          = 1_000_000
	minInterval        = 1_000
	maxInterval        = 86_400_000
	maxSubjectLen      = 128
	minRequestIDLen    = 16
	maxRequestIDLen    = 128
	maxAllowedPolicies = 100
)

var (
	policyIDRe  = regexp.MustCompile(`^[a-z][a-z0-9_]{2,47}$`)
	subjectRe   = regexp.MustCompile(`^[A-Za-z0-9._:@/-]+$`)
	requestIDRe = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)
)

func validatePolicyID(id string) error {
	if !policyIDRe.MatchString(id) {
		return fmt.Errorf("policy id must match %s", policyIDRe.String())
	}
	return nil
}

func validateName(name string, max int) (string, error) {
	trimmed := strings.TrimSpace(name)
	if n := len([]rune(trimmed)); n < minName || n > max {
		return "", fmt.Errorf("name must be %d to %d characters", minName, max)
	}
	return trimmed, nil
}

// validatePolicyFields validates the mutable numeric fields of a policy.
func validatePolicyFields(capacity, refill, interval, maxCost int64) error {
	if capacity < minCapacity || capacity > maxCapacity {
		return fmt.Errorf("capacity_tokens must be %d to %d", minCapacity, maxCapacity)
	}
	if refill < minRefill || refill > maxRefill {
		return fmt.Errorf("refill_tokens must be %d to %d", minRefill, maxRefill)
	}
	if interval < minInterval || interval > maxInterval {
		return fmt.Errorf("refill_interval_ms must be %d to %d", minInterval, maxInterval)
	}
	if maxCost < 1 || maxCost > capacity {
		return fmt.Errorf("max_cost_tokens must be 1 to capacity (%d)", capacity)
	}
	return nil
}

func validateSubject(subject string) error {
	if n := len(subject); n < 1 || n > maxSubjectLen {
		return fmt.Errorf("subject must be 1 to %d characters", maxSubjectLen)
	}
	if !subjectRe.MatchString(subject) {
		return fmt.Errorf("subject contains invalid characters")
	}
	return nil
}

func validateRequestID(id string) error {
	if n := len(id); n < minRequestIDLen || n > maxRequestIDLen {
		return fmt.Errorf("request id must be %d to %d characters", minRequestIDLen, maxRequestIDLen)
	}
	if !requestIDRe.MatchString(id) {
		return fmt.Errorf("request id contains invalid characters")
	}
	return nil
}

// ValidateDecisionInput validates the public fields of a decision request. It is
// the API-facing pre-check that lets the handler return 422 before proposing;
// the state machine re-validates authoritatively (including cost vs policy max).
func ValidateDecisionInput(policyID, subject, requestID string, cost int64) error {
	if err := validatePolicyID(policyID); err != nil {
		return err
	}
	if err := validateSubject(subject); err != nil {
		return err
	}
	if err := validateRequestID(requestID); err != nil {
		return err
	}
	if cost < 1 {
		return fmt.Errorf("cost must be at least 1")
	}
	return nil
}

// validateAllowedPolicies checks the allowed-policy list: either exactly ["*"]
// or 1..100 unique policy IDs.
func validateAllowedPolicies(ids []string) error {
	if len(ids) == 0 {
		return fmt.Errorf("allowed_policy_ids must not be empty")
	}
	if len(ids) == 1 && ids[0] == "*" {
		return nil
	}
	if len(ids) > maxAllowedPolicies {
		return fmt.Errorf("allowed_policy_ids must be at most %d", maxAllowedPolicies)
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "*" {
			return fmt.Errorf(`"*" cannot be combined with specific policy ids`)
		}
		if err := validatePolicyID(id); err != nil {
			return fmt.Errorf("allowed policy %q invalid: %w", id, err)
		}
		if seen[id] {
			return fmt.Errorf("allowed_policy_ids has duplicate %q", id)
		}
		seen[id] = true
	}
	return nil
}
