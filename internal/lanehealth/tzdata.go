package lanehealth

// Providers state the reset zone as an IANA name ("resets 10:50am
// (America/Chicago)"), so resolving that name is part of reading a banner
// correctly - reading it in the host's zone instead can roll a reset that has
// already passed forward a full day.
//
// The embedded database is what makes that work everywhere: Go has no platform
// zone source on Windows, where the daemon is a first-class target, so a host
// without a Go installation or ZONEINFO cannot resolve any IANA name. The
// system database still wins when there is one; this is only the fallback, and
// it is imported here rather than in package main so every binary and test
// that links the classifier has the capability the classifier depends on.
import _ "time/tzdata"
