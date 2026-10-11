package dashi

// stepVersion gates initial context even when an older JSON mirror strips
// unknown fields. Context-bearing version 1 and stripped version 2 both fail
// replay. Raw record readers do not interpret this execution contract.
func stepVersion(step Step) int {
	if step.Kind == StepTurnStarted && step.Started != nil &&
		len(step.Started.Context) > 0 {
		return 2
	}
	return 1
}
