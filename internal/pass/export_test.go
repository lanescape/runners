package pass

// OnRenewal sets the function r calls after each renewal that succeeds.
func OnRenewal(r *Runner, f func()) { r.renewed = f }

// OnStop sets the function r calls once Stop has marked it stopping.
func OnStop(r *Runner, f func()) { r.stopMarked = f }
