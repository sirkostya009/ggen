package main

// Dry-run / vet-friendly validation entry points. checkPackage and
// checkFile mirror generateDir / generateSingleFile but stop after
// parsing: no codegen, no buffer, no file write. The parse layer
// already surfaces every diagnostic the codegen path would have hit
// (applicability, custom-func resolution, tag-shape errors); both
// entry points just return the errors.Join the parser produced, and
// the caller's logger renders them.
//
// Designed so a future `ggenvet` static-analysis binary can reuse the
// same checks without dragging in any of the codegen surface. Vet-
// specific checks that need more than parse-time data (stale
// generated file detection, blame-able TODOs, post-CLI flag-state
// audits) can plug in here behind the applyCLIFlags call.

// checkPackage validates every annotated struct in dir. Returns the
// parser's errors.Join unchanged so the caller's logger can unwrap
// + render each sub-error individually. Trace + debug log lines
// mirror generateDir so verbose runs read the same in dry mode.
func checkPackage(dir string) error {
	cliLog.Debug("checking package %s", dir)
	structs, pkgName, err := parsePackage(dir)
	if err != nil {
		return err
	}
	if len(structs) == 0 {
		cliLog.Trace("no annotated structs in %s; skipping", dir)
		return nil
	}
	cliLog.Debug("package %s: %d annotated structs", pkgName, len(structs))
	// Run applyCLIFlags for parity with generateDir — a future vet check
	// that inspects post-CLI flag state (e.g. spotting -multierr +
	// -allowdups interactions) sees the same struct shape codegen would.
	// Pure config mutation, no error path.
	applyCLIFlags(structs)
	cliLog.Info("ok %s (%d structs)", relPath(dir), len(structs))
	return nil
}

// checkFile is the single-file analogue of checkPackage. `wanted` is
// the optional positional-name filter, identical to
// generateSingleFile's equivalent argument.
func checkFile(filename string, wanted []string) error {
	structs, _, _, err := parseFile(filename, wanted)
	if err != nil {
		return err
	}
	applyCLIFlags(structs)
	cliLog.Info("ok %s (%d structs)", relPath(filename), len(structs))
	return nil
}
