package shapes

//ggen:generate
type ByteArray struct {
	Fixed [4]byte `json:"fixed,format:array" pipe:"inner:(gte=1 oneof=1|2)"`
}
