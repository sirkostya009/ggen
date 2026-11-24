package shapes

// Proto has fields named like Object.prototype members.
//
//ggen:generate
type Proto struct {
	P string `json:"__proto__"`
	C string `json:"constructor"`
	A string `json:"a"`
}
