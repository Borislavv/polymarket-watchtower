package anomaly

// StrategyIdentity is the code-owned dedup namespace for this build. It is
// woven into every alert's dedup_key (single, cluster, accumulation) so
// alerts produced by different decision logic cannot collide.
//
// The identity is a property of the SOURCE CODE, not of operator config.
// Operators must not be able to flip the dedup namespace at runtime — a
// stray env var pointing at "v2" would silently re-alert on trades that
// the persisted history already de-duplicated. This is why STRATEGY_VERSION
// is no longer an environment variable.
//
// Bump this when the decision logic changes materially:
//   - tier thresholds → no bump (operator tuning, dedup is per-trade key)
//   - new detector wired in / removed → BUMP
//   - dedup-key format change → BUMP
//   - scorer formula change → BUMP
//
// Naming convention: "<product>-v<N>". v1..v4 were env-driven; the move to
// a code-owned constant is treated as the v4 generation since the decision
// logic itself did not change at this cleanup.
const StrategyIdentity = "informed-flow-v6"
