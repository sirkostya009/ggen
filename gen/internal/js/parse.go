package js

import "strings"

// isTimeHelper validates like time.Parse, a port of its layout walk without
// building the time.
const timeEncoder = "const ggenTimeEncoder = new TextEncoder();"

// isDateOnlyHelper, isTimeOnlyHelper and isDateTimeHelperLocal are the fixed
// layouts common enough to be worth a regex: ggenIsTime walks the layout on
// every call, which costs ~50x more. Each accepts exactly what time.Parse
// does for its layout, including the fractional seconds time.Parse takes even
// when the layout has none, and the space run its skip() folds.
const isDateOnlyHelper = `function ggenIsDateOnly(s: string): boolean {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(s);
  if (m === null) return false;
  const month = Number(m[2]);
  const day = Number(m[3]);
  return month >= 1 && month <= 12 && day >= 1 && day <= ggenDaysIn(Number(m[1]), month);
}`

const isTimeOnlyHelper = `function ggenIsTimeOnly(s: string): boolean {
  const m = /^(\d{1,2}):(\d{2}):(\d{2})(?:[.,]\d+)?$/.exec(s);
  return m !== null && Number(m[1]) < 24 && Number(m[2]) < 60 && Number(m[3]) < 60;
}`

const isDateTimeHelperLocal = `function ggenIsDateTimeLocal(s: string): boolean {
  const m = /^(\d{4})-(\d{2})-(\d{2}) +(\d{1,2}):(\d{2}):(\d{2})(?:[.,]\d+)?$/.exec(s);
  if (m === null) return false;
  const month = Number(m[2]);
  const day = Number(m[3]);
  return (
    month >= 1 &&
    month <= 12 &&
    day >= 1 &&
    day <= ggenDaysIn(Number(m[1]), month) &&
    Number(m[4]) < 24 &&
    Number(m[5]) < 60 &&
    Number(m[6]) < 60
  );
}`

const isTimeHelper = `function ggenIsTime(layout: string, value: string): boolean {
  const l = ggenTimeEncoder.encode(layout);
  const v = ggenTimeEncoder.encode(value);
  const isDigit = (s: Uint8Array, i: number) => i < s.length && s[i] >= 48 && s[i] <= 57;
  const has = (s: Uint8Array, i: number, t: string) => {
    if (i + t.length > s.length) return false;
    for (let k = 0; k < t.length; k++) if (s[i + k] !== t.charCodeAt(k)) return false;
    return true;
  };
  const at = (k: number) => (k < l.length ? String.fromCharCode(l[k]) : "");
  const chunk = (start: number, end: number, std: string) => ({ start, end, std });
  const next = (i: number) => {
    for (; i < l.length; i++) {
      switch (at(i)) {
        case "J":
          if (has(l, i, "January")) return chunk(i, i + 7, "January");
          if (has(l, i, "Jan") && !/[a-z]/.test(at(i + 3))) return chunk(i, i + 3, "Jan");
          break;
        case "M":
          if (has(l, i, "Monday")) return chunk(i, i + 6, "Monday");
          if (has(l, i, "Mon") && !/[a-z]/.test(at(i + 3))) return chunk(i, i + 3, "Mon");
          if (has(l, i, "MST")) return chunk(i, i + 3, "MST");
          break;
        case "0":
          if (/[1-6]/.test(at(i + 1))) return chunk(i, i + 2, "0" + at(i + 1));
          if (has(l, i, "002")) return chunk(i, i + 3, "002");
          break;
        case "1":
          return has(l, i, "15") ? chunk(i, i + 2, "15") : chunk(i, i + 1, "1");
        case "2":
          return has(l, i, "2006") ? chunk(i, i + 4, "2006") : chunk(i, i + 1, "2");
        case "_":
          if (has(l, i, "_2006")) return chunk(i + 1, i + 5, "2006");
          if (has(l, i, "_2")) return chunk(i, i + 2, "_2");
          if (has(l, i, "__2")) return chunk(i, i + 3, "__2");
          break;
        case "3":
        case "4":
        case "5":
          return chunk(i, i + 1, at(i));
        case "P":
        case "p":
          if (has(l, i, "PM") || has(l, i, "pm")) return chunk(i, i + 2, at(i) + at(i + 1));
          break;
        case "-":
        case "Z":
          for (const t of ["070000", "07:00:00", "0700", "07:00", "07"]) {
            if (has(l, i + 1, t)) return chunk(i, i + 1 + t.length, at(i) + t);
          }
          break;
        case ".":
        case ",": {
          const c = at(i + 1);
          if (c !== "0" && c !== "9") break;
          let j = i + 1;
          while (at(j) === c) j++;
          if (!isDigit(l, j)) return chunk(i, j, "." + c.repeat(j - i - 1));
          break;
        }
      }
    }
    return chunk(l.length, l.length, "");
  };
  let vi = 0;
  const getnum = (fixed: boolean): number => {
    if (!isDigit(v, vi)) return -1;
    if (!isDigit(v, vi + 1)) return fixed ? -1 : v[vi++] - 48;
    vi += 2;
    return (v[vi - 2] - 48) * 10 + v[vi - 1] - 48;
  };
  const atoi = (a: number, b: number): number | null => {
    let neg = false;
    if (a < b && (v[a] === 43 || v[a] === 45)) neg = v[a++] === 45;
    let x = 0;
    for (; a < b; a++) {
      if (!isDigit(v, a)) return null;
      x = x * 10 + v[a] - 48;
    }
    return neg ? -x : x;
  };
  const lookup = (names: string[]): number => {
    for (let k = 0; k < names.length; k++) {
      const n = names[k];
      let ok = vi + n.length <= v.length;
      for (let j = 0; ok && j < n.length; j++) {
        const c1 = v[vi + j] | 32;
        ok = v[vi + j] === n.charCodeAt(j) || (c1 === (n.charCodeAt(j) | 32) && c1 >= 97 && c1 <= 122);
      }
      if (ok) {
        vi += n.length;
        return k;
      }
    }
    return -1;
  };
  const zone = (): number => {
    const offset = (i: number): number => {
      if (v[vi + i] !== 43 && v[vi + i] !== 45) return 0;
      let k = i + 1;
      for (let x = 0; isDigit(v, vi + k); k++) {
        x = x * 10 + v[vi + k] - 48;
        if (x > 23) return 0;
      }
      return k === i + 1 ? 0 : k - i;
    };
    if (v.length - vi < 3) return 0;
    if (has(v, vi, "ChST") || has(v, vi, "MeST")) return 4;
    if (has(v, vi, "GMT")) return v.length - vi === 3 ? 3 : 3 + offset(3);
    if (v[vi] === 43 || v[vi] === 45) return offset(0);
    let upper = 0;
    while (upper < 6 && v[vi + upper] >= 65 && v[vi + upper] <= 90) upper++;
    if (upper === 3) return 3;
    if (upper === 4 && (v[vi + 3] === 84 || has(v, vi, "WITA"))) return 4;
    return upper === 5 && v[vi + 4] === 84 ? 5 : 0;
  };
  const months = ["January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"];
  const days = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];
  let year = 0;
  let month = -1;
  let day = -1;
  let yday = -1;
  for (let li = 0; ; ) {
    const { start, end, std } = next(li);
    for (let p = li; p < start; ) {
      if (l[p] === 32) {
        if (vi < v.length && v[vi] !== 32) return false;
        while (p < start && l[p] === 32) p++;
        while (vi < v.length && v[vi] === 32) vi++;
      } else {
        if (v[vi] !== l[p]) return false;
        p++;
        vi++;
      }
    }
    if (std === "") {
      if (vi !== v.length) return false;
      break;
    }
    li = end;
    switch (std) {
      case "06": {
        const y = v.length - vi < 2 ? null : atoi(vi, vi + 2);
        if (y === null) return false;
        vi += 2;
        year = y >= 69 ? y + 1900 : y + 2000;
        break;
      }
      case "2006": {
        const y = v.length - vi < 4 || !isDigit(v, vi) ? null : atoi(vi, vi + 4);
        if (y === null) return false;
        vi += 4;
        year = y;
        break;
      }
      case "Jan":
      case "January":
        month = lookup(std === "Jan" ? months.map((m) => m.slice(0, 3)) : months) + 1;
        if (month === 0) return false;
        break;
      case "1":
      case "01":
        month = getnum(std === "01");
        if (month < 1 || month > 12) return false;
        break;
      case "Mon":
      case "Monday":
        if (lookup(std === "Mon" ? days.map((d) => d.slice(0, 3)) : days) < 0) return false;
        break;
      case "2":
      case "_2":
      case "02":
        if (std === "_2" && v[vi] === 32) vi++;
        day = getnum(std === "02");
        if (day < 0) return false;
        break;
      case "__2":
      case "002": {
        for (let k = 0; k < 2 && std === "__2" && v[vi] === 32; k++) vi++;
        let n = 0;
        for (yday = 0; n < 3 && isDigit(v, vi + n); n++) yday = yday * 10 + v[vi + n] - 48;
        if (n === 0 || (std === "002" && n !== 3)) return false;
        vi += n;
        break;
      }
      case "15": {
        const h = getnum(false);
        if (h < 0 || h >= 24) return false;
        break;
      }
      case "3":
      case "03": {
        const h = getnum(std === "03");
        if (h < 0 || h > 12) return false;
        break;
      }
      case "4":
      case "04":
      case "5":
      case "05": {
        const n = getnum(std.length === 2);
        if (n < 0 || n >= 60) return false;
        const frac = v.length - vi >= 2 && (v[vi] === 46 || v[vi] === 44) && isDigit(v, vi + 1);
        if (std.endsWith("5") && frac && !next(li).std.startsWith(".")) {
          vi += 2;
          while (isDigit(v, vi)) vi++;
        }
        break;
      }
      case "PM":
      case "pm": {
        const p = v.length - vi < 2 ? "" : String.fromCharCode(v[vi], v[vi + 1]);
        vi += 2;
        if (p !== std && p !== (std === "PM" ? "AM" : "am")) return false;
        break;
      }
      case "MST": {
        const n = has(v, vi, "UTC") ? 3 : zone();
        if (n === 0) return false;
        vi += n;
        break;
      }
      default: {
        if (std.startsWith(".")) {
          const n = std.length - 1;
          const sep = v[vi] === 46 || v[vi] === 44;
          if (std[1] === "9") {
            if (v.length - vi >= 2 && sep && isDigit(v, vi + 1)) for (vi++; isDigit(v, vi); vi++);
          } else {
            const ns = v.length - vi < 1 + n || !sep ? null : atoi(vi + 1, vi + Math.min(1 + n, 10));
            if (ns === null || ns < 0) return false;
            vi += 1 + n;
          }
          break;
        }
        if (std[0] === "Z" && v[vi] === 90) {
          vi++;
          break;
        }
        const layout = std.slice(1);
        if (v.length - vi < layout.length + 1 || (v[vi] !== 43 && v[vi] !== 45)) return false;
        const limits = [24, 60, 60];
        for (let k = 0, f = 0; k < layout.length; k += 2, f++) {
          if (layout[k] === ":") {
            if (v[vi + 1 + k] !== 58) return false;
            k++;
          }
          if (!isDigit(v, vi + 1 + k) || !isDigit(v, vi + 2 + k)) return false;
          if ((v[vi + 1 + k] - 48) * 10 + v[vi + 2 + k] - 48 > limits[f]) return false;
        }
        vi += layout.length + 1;
      }
    }
  }
  if (yday >= 0) {
    const before = [0, 31, 59, 90, 120, 151, 181, 212, 243, 273, 304, 334, 365];
    let m = 0;
    let d = 0;
    if (ggenDaysIn(year, 2) === 29) {
      if (yday === 60) {
        m = 2;
        d = 29;
      } else if (yday > 60) yday--;
    }
    if (yday < 1 || yday > 365) return false;
    if (m === 0) {
      m = Math.floor((yday - 1) / 31) + 1;
      if (before[m] < yday) m++;
      d = yday - before[m - 1];
    }
    if ((month >= 0 && month !== m) || (day >= 0 && day !== d)) return false;
    month = m;
    day = d;
  }
  if (month < 0) month = 1;
  if (day < 0) day = 1;
  return day >= 1 && day <= ggenDaysIn(year, month);
}`

const daysInHelper = `function ggenDaysIn(year: number, month: number): number {
  if (month !== 2) return 30 + ((month + (month >> 3)) & 1);
  return year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0) ? 29 : 28;
}`

// isDateTimeHelper is ggen's strict RFC 3339.
const isDateTimeHelper = `function ggenIsDateTime(s: string): boolean {
  const m = /^(\d{4})-(0[1-9]|1[0-2])-(0[1-9]|[12]\d|3[01])T(?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d(?:\.\d+)?(?:Z|[+-](?:[01]\d|2[0-3]):[0-5]\d)$/.exec(s);
  return m !== null && Number(m[3]) <= ggenDaysIn(Number(m[1]), Number(m[2]));
}`

// isBigFloatHelper validates like big.Float.Parse(s, 10): the grammar, and
// a binary exponent within int32.
const isBigFloatHelper = `function ggenIsBigFloat(s: string): boolean {
  const m = /^[+-]?(?:[Ii]nf|(\d*)(?:\.(\d*))?(?:[eEpP]([+-]?\d+))?)$/.exec(s);
  if (m === null) return false;
  const [whole, int = "", frac = "", exp = "0"] = m;
  if (/inf$/i.test(whole)) return true;
  if (int === "" && frac === "") return false;
  const e = BigInt(exp);
  if (e < -(2n ** 63n) || e >= 2n ** 63n) return false;
  const mant = BigInt(int + frac || "0");
  if (mant === 0n) return true;
  const exp2 = BigInt(mant.toString(2).length - frac.length) + e;
  return exp2 >= -(2n ** 31n) && exp2 < 2n ** 31n;
}`

// isRationalHelper validates like big.Rat.SetString: base-prefixed integers
// and fractions with _ separators, and exponents within its limits.
var isRationalHelper = func() string {
	seq := func(d string) string { return d + "(?:_?" + d + ")*" }
	digits := map[string]string{"x": "[0-9a-fA-F]", "b": "[01]", "o": "[0-7]"}
	nat := make([]string, 0, 5)
	mant := make([]string, 0, 4)
	for _, p := range []string{"x", "b", "o"} {
		d := digits[p]
		prefix := "0[" + p + strings.ToUpper(p) + "]"
		nat = append(nat, prefix+"_?"+seq(d))
		mant = append(mant, prefix+"(?:_?"+seq(d)+`(?:\.(?:`+seq(d)+`)?)?|\.`+seq(d)+")")
	}
	nat = append(nat, `0(?:_?[0-7])*`, `[1-9](?:_?\d)*`)
	mant = append(mant, `(?:`+seq(`\d`)+`(?:\.(?:`+seq(`\d`)+`)?)?|\.`+seq(`\d`)+")")
	n := "(?:" + strings.Join(nat, "|") + ")"
	re := `^[-+]?(?:` + n + "/" + n + "|(?:" + strings.Join(mant, "|") + `)(?:[eEpP][-+]?` + seq(`\d`) + ")?)$"
	return `function ggenIsRational(s: string): boolean {
  if (!` + Regex(re, "") + `.test(s)) return false;
  const slash = s.indexOf("/");
  if (slash >= 0) return /[1-9a-fA-F]/.test(s.slice(slash + 1).replace(/^0[xXbBoO]/, ""));
  const body = s.replace(/^[-+]/, "").replaceAll("_", "");
  const bits = ({ "0x": 4, "0b": 1, "0o": 3 } as Record<string, number>)[body.slice(0, 2).toLowerCase()] ?? 0;
  const digits = bits > 0 ? body.slice(2) : body;
  const at = digits.search(bits === 4 ? /[pP]/ : /[eEpP]/);
  const mant = at < 0 ? digits : digits.slice(0, at);
  const e = at < 0 ? 0n : BigInt(digits.slice(at + 1));
  if (e < -(2n ** 63n) || e >= 2n ** 63n) return false;
  const [int, frac = ""] = mant.split(".");
  if (BigInt((bits === 4 ? "0x" : bits === 1 ? "0b" : bits === 3 ? "0o" : "") + (int + frac || "0")) === 0n) return true;
  const point = BigInt(-frac.length);
  const decimal = at >= 0 && /[eE]/.test(digits[at]);
  const exp5 = (bits === 0 ? point : 0n) + (decimal ? e : 0n);
  const exp2 = (bits === 0 ? point : point * BigInt(bits)) + e;
  return exp5 <= 1000000n && exp5 >= -1000000n && exp2 <= 10000000n && exp2 >= -10000000n;
}`
}()

// parsesURLHelper validates like url.Parse, with strict colons in http and
// https hosts.
const parsesURLHelper = `function ggenParsesURL(raw: string): boolean {
  const hostByte = (c: string) => /[A-Za-z0-9\-_.~!$&'()*+,;=:[\]<>"]/.test(c);
  const unescape = (s: string, mode: "" | "host" | "zone"): string | null => {
    let out = "";
    for (let i = 0; i < s.length; ) {
      if (s[i] === "%") {
        if (!/^%[0-9a-fA-F]{2}$/.test(s.slice(i, i + 3))) return null;
        const b = parseInt(s.slice(i + 1, i + 3), 16);
        if (mode === "host" && b < 0x80 && s.slice(i, i + 3) !== "%25") return null;
        if (mode === "zone" && s.slice(i, i + 3) !== "%25" && b !== 32 && (b >= 0x80 || !hostByte(String.fromCharCode(b)))) return null;
        out += String.fromCharCode(b);
        i += 3;
      } else {
        if (mode !== "" && s.charCodeAt(i) < 0x80 && !hostByte(s[i])) return null;
        out += s[i++];
      }
    }
    return out;
  };
  const port = (p: string) => /^(?::\d*)?$/.test(p);
  const host = (scheme: string, h: string): boolean => {
    const open = h.lastIndexOf("[");
    if (open > 0) return false;
    if (open === 0) {
      const close = h.lastIndexOf("]");
      if (close < 0 || !port(h.slice(close + 1))) return false;
      const name = h.slice(1, close);
      const zone = name.indexOf("%25");
      const ip = unescape(zone < 0 ? name : name.slice(0, zone), "host");
      const id = zone < 0 ? "" : unescape(name.slice(zone), "zone");
      return ip !== null && id !== null && ip.includes(":") && ggenIsAddr(ip + id);
    }
    const colon = h.indexOf(":");
    if (colon >= 0) {
      const i = h.lastIndexOf(":") !== colon && scheme !== "http" && scheme !== "https" ? h.lastIndexOf(":") : colon;
      if (!port(h.slice(i))) return false;
    }
    return unescape(h, "host") !== null;
  };
  const hash = raw.indexOf("#");
  let rest = hash < 0 ? raw : raw.slice(0, hash);
  if (hash >= 0 && unescape(raw.slice(hash + 1), "") === null) return false;
  if (/[\x00-\x1f\x7f]/.test(rest)) return false;
  if (rest === "*") return true;
  const m = /^[A-Za-z][A-Za-z0-9+\-.]*:/.exec(rest);
  if (rest[0] === ":") return false;
  const scheme = m === null ? "" : m[0].slice(0, -1).toLowerCase();
  if (m !== null) rest = rest.slice(m[0].length);
  const q = rest.indexOf("?");
  if (q >= 0) rest = rest.slice(0, q);
  if (!rest.startsWith("/")) {
    if (scheme !== "") return true;
    if (rest.split("/")[0].includes(":")) return false;
  }
  if ((scheme !== "" || !rest.startsWith("///")) && rest.startsWith("//")) {
    let authority = rest.slice(2);
    const slash = authority.indexOf("/");
    rest = slash < 0 ? "" : authority.slice(slash);
    if (slash >= 0) authority = authority.slice(0, slash);
    const at = authority.lastIndexOf("@");
    if (!host(scheme, authority.slice(at + 1))) return false;
    if (at >= 0 && (!/^[A-Za-z0-9\-._:~!$&'()*+,;=%@]*$/.test(authority.slice(0, at)) || unescape(authority.slice(0, at), "") === null)) return false;
  }
  return unescape(rest, "") !== null;
}`
