package application

// Decision is the explicit UnitOfWork finish. The adapter commits or
// rolls back from this value. It must not inspect error text or codes.
type Decision int

const (
	// Rollback discards the transaction: infrastructure errors, context
	// cancellation, and validation that made no security-critical writes.
	Rollback Decision = iota
	// Commit persists the transaction: success, and business rejections
	// whose side effects must survive (rate limit, lockout, token reuse).
	Commit
)

// Finish contract for the PostgreSQL adapter (not implemented here):
//
//	Register success                         Commit
//	Register AUTH_RATE_LIMITED               Commit (counter)
//	Register EMAIL_TAKEN                     Commit (counter; no user row)
//	Register INVALID_REQUEST (no writes)     Rollback
//	Register hasher/store/ctx error          Rollback
//	Login success                            Commit
//	Login INVALID_CREDENTIALS                Commit (counter, attempt, failures, lock)
//	Login AUTH_RATE_LIMITED                  Commit
//	Login hasher/store/ctx error             Rollback
//	Refresh success                          Commit
//	Refresh TOKEN_REUSED                     Commit (session + all refresh revoked)
//	Refresh TOKEN_EXPIRED                    Commit (counter; session stays live)
//	Refresh INVALID_TOKEN (unknown token)    Commit (counter; other sessions untouched)
//	Refresh INVALID_REQUEST                  Rollback
//	Refresh store/issuer/ctx error           Rollback
//	Logout success / repeat logout           Commit
//	Logout INVALID_TOKEN                     Rollback
//	Grant/Revoke success                     Commit
//	Grant/Revoke FORBIDDEN/NOT_FOUND/INVALID Rollback
//	Grant/Revoke store/ctx error             Rollback
//	Lock/Unlock/Disable success / idempotent Commit
//	Lock/Disable last ACTIVE ADMIN           Rollback
//	Lock/Unlock/Disable FORBIDDEN/NOT_FOUND  Rollback
//	Unlock DISABLED (invalid transition)     Rollback
//	Login last ACTIVE ADMIN at LockAfter     Commit (attempt + fail count; not LOCKED)
//
// GetForUpdate on a refresh hash must be SELECT … FOR UPDATE held until
// the transaction ends so two concurrent Refresh calls cannot both observe
// an unused token. Unit tests serialize the fake transaction; they do not
// prove PostgreSQL isolation.
