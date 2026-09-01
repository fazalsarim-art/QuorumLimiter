package limiter

import "testing"

// bucket is a tiny constructor for pure-math tests.
func bucket(tokensMilli, lastRefill, remainder int64) TokenBucketState {
	return TokenBucketState{TokensMilli: tokensMilli, LastRefillMS: lastRefill, RefillRemainder: remainder}
}

func TestTokenBucketZeroElapsedNoChange(t *testing.T) {
	b := bucket(3000, 1000, 250)
	refillBucket(&b, 5, 1, 12000, 1000) // now == last_refill
	if b.TokensMilli != 3000 || b.RefillRemainder != 250 || b.LastRefillMS != 1000 {
		t.Errorf("zero elapsed changed state: %+v", b)
	}
}

func TestTokenBucketBackwardTimeDoesNotDrain(t *testing.T) {
	b := bucket(3000, 10000, 0)
	refillBucket(&b, 5, 1, 12000, 5000) // now < last_refill
	if b.LastRefillMS != 10000 {
		t.Errorf("LastRefillMS moved backward to %d", b.LastRefillMS)
	}
	if b.TokensMilli != 3000 {
		t.Errorf("tokens changed on backward time: %d", b.TokensMilli)
	}
}

func TestTokenBucketRemainderPreservedAcrossSteps(t *testing.T) {
	// Refilling in two steps must equal refilling in one step (no lost credit).
	cap, refill, interval := int64(1000000), int64(1), int64(12000)

	stepwise := bucket(0, 0, 0)
	refillBucket(&stepwise, cap, refill, interval, 5000)
	refillBucket(&stepwise, cap, refill, interval, 10000)

	oneShot := bucket(0, 0, 0)
	refillBucket(&oneShot, cap, refill, interval, 10000)

	if stepwise.TokensMilli != oneShot.TokensMilli || stepwise.RefillRemainder != oneShot.RefillRemainder {
		t.Errorf("stepwise %+v != oneShot %+v (remainder not preserved)", stepwise, oneShot)
	}
}

func TestTokenBucketReachingCapacityResetsRemainder(t *testing.T) {
	b := bucket(4000, 0, 500) // near capacity with leftover remainder
	refillBucket(&b, 5, 1, 12000, 1_000_000_000)
	if b.TokensMilli != 5*milliPerToken {
		t.Errorf("tokens = %d, want clamped to %d", b.TokensMilli, 5*milliPerToken)
	}
	if b.RefillRemainder != 0 {
		t.Errorf("remainder = %d, want reset to 0 at capacity", b.RefillRemainder)
	}
}

func TestTokenBucketOverflowClampsToCapacity(t *testing.T) {
	b := bucket(0, 0, 0)
	refillBucket(&b, 5, 1_000_000, 1000, 1<<62) // enormous elapsed -> overflow path
	if b.TokensMilli != 5*milliPerToken {
		t.Errorf("tokens = %d, want %d", b.TokensMilli, 5*milliPerToken)
	}
}

func TestTokenBucketSpend(t *testing.T) {
	// Full 5-token bucket allows a cost-1 spend and denies when short.
	b := bucket(5*milliPerToken, 0, 0)
	if allowed, retry := spend(&b, 1, 1, 12000); !allowed || retry != 0 {
		t.Fatalf("cost 1 on full bucket: allowed=%v retry=%d", allowed, retry)
	}
	if b.TokensMilli != 4*milliPerToken {
		t.Errorf("after spend tokens = %d, want 4000", b.TokensMilli)
	}

	empty := bucket(0, 0, 0)
	allowed, retry := spend(&empty, 1, 1, 12000)
	if allowed {
		t.Error("empty bucket should deny")
	}
	if retry <= 0 {
		t.Errorf("denied retry_after = %d, want positive", retry)
	}
	if empty.TokensMilli != 0 {
		t.Errorf("denied spend changed balance to %d", empty.TokensMilli)
	}
}

func TestTokenBucketRetryAfterEstimate(t *testing.T) {
	// Need 1 whole token; refill 1 token / 12000ms; from empty -> ~12000ms.
	b := bucket(0, 0, 0)
	_, retry := spend(&b, 1, 1, 12000)
	if retry != 12000 {
		t.Errorf("retry_after = %d, want 12000", retry)
	}
}

func TestCheckedArithmetic(t *testing.T) {
	if _, ovf := checkedMul(1<<40, 1<<40); !ovf {
		t.Error("expected overflow on large multiply")
	}
	if v, ovf := checkedMul(1000, 1000); ovf || v != 1_000_000 {
		t.Errorf("checkedMul(1000,1000) = %d ovf=%v", v, ovf)
	}
	const maxInt64 = int64(^uint64(0) >> 1)
	if _, ovf := checkedAdd(maxInt64, 1); !ovf {
		t.Error("expected overflow on add")
	}
}
