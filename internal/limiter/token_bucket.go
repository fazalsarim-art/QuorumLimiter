package limiter

// milliPerToken is the fixed-point scale: 1000 milli-tokens == 1 token.
const milliPerToken = 1000

// checkedMul multiplies two non-negative int64 values, reporting overflow.
func checkedMul(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, false
	}
	c := a * b
	if c/b != a {
		return 0, true
	}
	return c, false
}

// checkedAdd adds two non-negative int64 values, reporting overflow.
func checkedAdd(a, b int64) (int64, bool) {
	c := a + b
	if c < a {
		return 0, true
	}
	return c, false
}

// computeAdded returns the milli-tokens to add and the new division remainder
// for the given elapsed time, using integer math that preserves the remainder:
//
//	numerator = elapsed_ms * refill_tokens * 1000 + remainder
//	added     = numerator / refill_interval_ms
//	newRem    = numerator % refill_interval_ms
//
// overflow is true when the numerator cannot be represented; the caller then
// treats the bucket as filled to capacity (the only possible result after such
// a long idle period).
func computeAdded(elapsedMS, refillTokens, intervalMS, remainder int64) (added, newRem int64, overflow bool) {
	step1, ovf := checkedMul(elapsedMS, refillTokens)
	if ovf {
		return 0, 0, true
	}
	step2, ovf := checkedMul(step1, milliPerToken)
	if ovf {
		return 0, 0, true
	}
	numerator, ovf := checkedAdd(step2, remainder)
	if ovf {
		return 0, 0, true
	}
	return numerator / intervalMS, numerator % intervalMS, false
}

// refillBucket advances b to effective time under the given policy parameters
// using only integer arithmetic. Effective time is max(nowMS, last_refill), so
// time never moves backward after a leader change or clock correction. On
// reaching capacity the remainder resets, so stale fractional credit cannot push
// the balance over capacity.
func refillBucket(b *TokenBucketState, capacityTokens, refillTokens, intervalMS, nowMS int64) {
	capMilli := capacityTokens * milliPerToken // capacity <= 1e6 -> <= 1e9, safe

	effective := b.LastRefillMS
	if nowMS > effective {
		effective = nowMS
	}
	elapsed := effective - b.LastRefillMS // >= 0 by construction

	added, rem, overflow := computeAdded(elapsed, refillTokens, intervalMS, b.RefillRemainder)
	if overflow {
		b.TokensMilli = capMilli
		b.RefillRemainder = 0
		b.LastRefillMS = effective
		return
	}

	newTokens, addOvf := checkedAdd(b.TokensMilli, added)
	if addOvf || newTokens >= capMilli {
		b.TokensMilli = capMilli
		b.RefillRemainder = 0
	} else {
		b.TokensMilli = newTokens
		b.RefillRemainder = rem
	}
	b.LastRefillMS = effective
}

// spend attempts to deduct cost whole tokens from a freshly refilled bucket. On
// success it deducts and returns allowed=true. On failure the balance is
// unchanged and retryAfterMS is a positive integer estimate of when enough
// tokens will have accrued.
func spend(b *TokenBucketState, cost, refillTokens, intervalMS int64) (allowed bool, retryAfterMS int64) {
	costMilli := cost * milliPerToken
	if b.TokensMilli >= costMilli {
		b.TokensMilli -= costMilli
		return true, 0
	}
	needed := costMilli - b.TokensMilli
	rate := refillTokens * milliPerToken // milli-tokens per interval
	num, ovf := checkedMul(needed, intervalMS)
	if ovf {
		return false, intervalMS
	}
	// ceil(needed * interval / rate)
	retry := (num + rate - 1) / rate
	if retry <= 0 {
		retry = 1
	}
	return false, retry
}
