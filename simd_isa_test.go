//go:build goexperiment.simd && amd64 && linux

package ggen

import (
	"debug/elf"
	"debug/gosym"
	"os"
	"testing"
)

// TestSIMD_AVXTierIsAVXOnly reads the 16-lane tier's machine code from the
// test binary and fails on a VEX-encoded integer broadcast (VPBROADCASTB/W/D/Q,
// 0F38 58/59/78/79), which needs AVX2. Running the code proves nothing on a
// machine that has AVX2.
func TestSIMD_AVXTierIsAVXOnly(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	f, err := elf.Open(exe)
	if err != nil {
		t.Skip(err)
	}
	defer f.Close()
	text, pcln := f.Section(".text"), f.Section(".gopclntab")
	if text == nil || pcln == nil {
		t.Skip("no .text or .gopclntab section")
	}
	code, err := text.Data()
	if err != nil {
		t.Skip(err)
	}
	pclnData, err := pcln.Data()
	if err != nil {
		t.Skip(err)
	}
	tab, err := gosym.NewTable(nil, gosym.NewLineTable(pclnData, text.Addr))
	if err != nil {
		t.Skip(err)
	}
	const pkg = "github.com/sirkostya009/ggen."
	for _, name := range []string{
		"StringAVX", "structuralIndexAVX", "structuralIndexHighAVX", "skipSpaceAVXSlow",
		"AppendStringAVX", "AppendStringNoHTMLAVX", "validUTF8x16", "(*Stream).skipSpaceSlowAVX",
		"ElemCountAVX",
	} {
		fn := tab.LookupFunc(pkg + name)
		if fn == nil {
			t.Errorf("%s: not found in the test binary", name)
			continue
		}
		body := code[fn.Entry-text.Addr : fn.End-text.Addr]
		for i := 0; i+4 < len(body); i++ {
			if body[i] != 0xC4 || body[i+1]&0x1F != 0x02 || body[i+2]&0x7B != 0x79 {
				continue
			}
			switch body[i+3] {
			case 0x58, 0x59, 0x78, 0x79:
				t.Errorf("%s: AVX2 broadcast at +%#x", name, i)
			}
		}
	}
}
