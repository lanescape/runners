package pass

// OnRenewal sets the function r calls after each renewal that succeeds.
func OnRenewal(r *Runner, f func()) { r.renewed = f }
