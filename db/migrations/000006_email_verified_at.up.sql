-- users.email_verified_at: the gate on the whole backoffice.  (S1-084, BR-006)
--
-- One nullable column, not a new status. `status` already answers "is this
-- account usable at all" (invited | active | disabled); this answers "has the
-- person proved they hold the address". They are different questions and an
-- account can be active-but-unverified, which is exactly the state a fresh
-- registration lands in.
--
-- The reason is not filtering spam accounts -- that is a side effect. Email is
-- the only owner identity with any proof behind it: there is no business
-- verification and no KYC. An owner who fills 50 units and hundreds of
-- bookings into an unverified account has no recovery path at all, and one
-- forgotten password loses everything. Verification protects the owner's own
-- data.
ALTER TABLE users ADD COLUMN email_verified_at timestamptz;

-- Partial index: the verification middleware asks "is this one unverified" per
-- request, and once the fleet is mostly verified the NULLs are the small side.
CREATE INDEX users_unverified ON users (id) WHERE email_verified_at IS NULL;
