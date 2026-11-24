package main

import (
	"errors"
	"fmt"

	"github.com/sirkostya009/ggen/gen/model"
)

// Validation entry points for -dry. checkPackage and checkFile mirror
// generateDir and generateSingleFile but stop after parsing. They generate no
// code and write no file. The parser, applyFlags and the output naming
// report every error that code generation would hit, so these return that
// error as is.

// checkPackage validates every annotated struct in dir. It returns the
// parser's joined error unchanged, and the caller's logger unwraps and prints
// it.
func checkPackage(dir string) error {
	cliLog.Debugf("checking package %s", dir)
	pkg, err := model.ParsePackage(dir)
	if err != nil {
		return err
	}
	structs, pkgName := pkg.Structs, pkg.Name
	if len(structs) == 0 {
		cliLog.Tracef("no annotated structs in %s; skipping", dir)
		return nil
	}
	cliLog.Debugf("package %s: %d annotated structs", pkgName, len(structs))
	// Same as generateDir, so validation sees the same struct shape.
	if err := applyFlags(structs); err != nil {
		return err
	}
	if _, err := bucketFileNames(dir, bucketKeys(bucketStructs(structs))); err != nil {
		return err
	}
	cliLog.Infof("ok %s (%d structs)", model.RelPath(dir), len(structs))
	return nil
}

// checkFile is the single-file version of checkPackage. wanted is the
// optional list of struct names given on the command line.
func checkFile(filename string, wanted []string) error {
	res, err := model.ParseFile(filename, wanted)
	structs := res.Structs
	if err != nil {
		return err
	}
	if err := applyFlags(structs); err != nil {
		return err
	}
	cliLog.Infof("ok %s (%d structs)", model.RelPath(filename), len(structs))
	return nil
}

// applyFlags turns on the command-line flags for structs, then rejects a
// field named like a JSON hook the struct now gets. The parser rejects the
// names of the methods every generated struct has.
func applyFlags(structs []model.StructInfo) error {
	cliFlags.Apply(structs)
	var errs []error
	for _, s := range structs {
		for _, f := range s.Fields {
			if s.Marshal && f.GoName == "MarshalJSON" || s.Unmarshal && f.GoName == "UnmarshalJSON" {
				errs = append(errs, fmt.Errorf("%s.%s: a field cannot share its name with the generated %s method", s.Name, f.GoName, f.GoName))
			}
		}
	}
	return errors.Join(errs...)
}
