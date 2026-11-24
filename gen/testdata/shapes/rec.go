// Package shapes holds types that pin edge cases of the lowering.
package shapes

// Tree holds itself.
//
//ggen:generate
type Tree []Tree

// Obj holds itself.
//
//ggen:generate
type Obj map[string]Obj

// Ping and Pong hold each other.
//
//ggen:generate
type Ping []Pong

//ggen:generate
type Pong []Ping

// Nest is an unannotated container that holds itself.
type Nest []Nest

//ggen:generate
type Holder struct {
	N Nest `json:"n"`
}

// Bag collects undeclared keys of its own shape.
//
//ggen:generate
type Bag struct {
	A    int            `json:"a"`
	Rest map[string]Bag `json:",embed"`
}
