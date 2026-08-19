package ticket

// isWorkableStatus guards the desk's off-workflow override: the move may leave
// the configured graph, but it still has to name a state the product has.
// Without this the override would accept any string and write it straight onto
// the ticket.
func isWorkableStatus(status string) bool {
	for _, s := range WorkableStatuses() {
		if s == status {
			return true
		}
	}
	return false
}
