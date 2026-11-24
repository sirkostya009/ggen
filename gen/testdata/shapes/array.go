package shapes

// Length has a required field every array has.
//
//ggen:generate
type Length struct {
	N int `json:"length" pipe:"required"`
}

// Index has a required field an array of one element has.
//
//ggen:generate
type Index struct {
	First string `json:"0" pipe:"required"`
}

// Method has a required field named like an Array.prototype method.
//
//ggen:generate
type Method struct {
	M any `json:"map" pipe:"required"`
}

// Named has a required field no array has.
//
//ggen:generate
type Named struct {
	First string `json:"0" pipe:"required"`
	Name  string `json:"name" pipe:"required"`
}
