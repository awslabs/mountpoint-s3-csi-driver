package system

// OsPts is the mock Darwin implementation of the Pts interface.
type OsPts struct{}

func NewOsPts() Pts {
	panic("not supported on darwin")
}
