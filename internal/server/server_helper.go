package server

import "workweave/router/internal/api/admin"

func firstOrNil(checkers []admin.HealthChecker) admin.HealthChecker {
	if len(checkers) > 0 {
		return checkers[0]
	}
	return nil
}
