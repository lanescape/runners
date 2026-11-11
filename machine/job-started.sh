#!/bin/bash
# The job-started hook (specs/0002-machines.md, The job-started check), which
# GitHub's runner runs before the job's first step. It only runs the hook's
# program, from the service's own directory, which asks the service whether
# the job may run here: see hook/main.go. Tracing that the job's environment
# turns on, as SHELLOPTS=xtrace does, shows nothing but this line.
exec /opt/lanescape-runners/job-started "$@"
