package service

// checkedFixtureValue makes fixture shape failures explicit before mutation.
func checkedFixtureValue[T any](value any) T {
	result, ok := value.(T)
	if !ok {
		panic("invalid test fixture type")
	}
	return result
}
