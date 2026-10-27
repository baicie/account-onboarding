package api

import "time"

// SetMaintenanceDrainTimeoutForTest shortens the bounded maintenance drain so a
// test can observe the not-yet-quiescent state without waiting 30s. Production
// never calls it; the default stays in maintenanceDrainTimeout.
func SetMaintenanceDrainTimeoutForTest(d time.Duration) { maintenanceDrainTimeout = d }

// MaintenanceDrainTimeoutForTest returns the current drain bound.
func MaintenanceDrainTimeoutForTest() time.Duration { return maintenanceDrainTimeout }
