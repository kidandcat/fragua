import Lean
import FraguaGeom.Basic

/-
Length-prefixed JSON server for the geometry kernel.

Each request and response is a little-endian uint32 byte count followed by
that many UTF-8 bytes of one JSON object. Integers travel as decimal strings
so a squared distance (up to a few hundred bits for int64 endpoints) is not
rounded through a JSON number.

MIT License — see the repository LICENSE.
-/

open FraguaGeom
open Lean

def u32le (b : ByteArray) : Nat :=
  (b.get! 0).toNat + (b.get! 1).toNat * 256
    + (b.get! 2).toNat * 65536 + (b.get! 3).toNat * 16777216

def pushU32LE (n : Nat) : ByteArray := Id.run do
  let mut a := ByteArray.empty
  a := a.push <| UInt8.ofNat (n % 256)
  a := a.push <| UInt8.ofNat ((n / 256) % 256)
  a := a.push <| UInt8.ofNat ((n / 65536) % 256)
  a := a.push <| UInt8.ofNat ((n / 16777216) % 256)
  return a

def readExact (h : IO.FS.Stream) (n : Nat) : IO ByteArray := do
  let mut acc := ByteArray.empty
  let mut left := n
  while left > 0 do
    let chunk ← h.read (USize.ofNat left)
    if chunk.isEmpty then
      return acc
    acc := acc ++ chunk
    left := left - chunk.size
  return acc

def bytesToString (b : ByteArray) : Except String String :=
  match String.fromUTF8? b with
  | some s => .ok s
  | none => .error "payload is not utf-8"

def parseInt (s : String) : Except String Int :=
  match s.toInt? with
  | some n => .ok n
  | none => .error s!"not an integer: {s}"

def field (j : Json) (k : String) : Except String Int := do
  let v ← j.getObjVal? k
  let s ← v.getStr?
  parseInt s

def point2 (j : Json) (xk yk : String) : Except String Point := do
  return ⟨← field j xk, ← field j yk⟩

def ratOut (r : Rat2) : Json :=
  let r := r.normalize
  Json.mkObj [("num", Json.str (toString r.num)), ("den", Json.str (toString r.den))]

def errOut (s : String) : Json :=
  Json.mkObj [("err", Json.str s)]

/-- Sum of normalized segment-distance numerators.
The index changes on every iteration, so the distance is not loop-invariant
and a compiler that hoists the call would change the result. -/
def segSum (n : Nat) : Int := Id.run do
  let mut acc : Int := 0
  for i in [0:n] do
    let k : Int := Int.ofNat i
    let a0 : Point := ⟨k, 0⟩
    let a1 : Point := ⟨k + 5000000, Int.ofNat (i % 17)⟩
    let b0 : Point := ⟨0, 250000⟩
    let b1 : Point := ⟨8000000, 250000 + Int.ofNat (i % 5)⟩
    let r := (segSegDist2 a0 a1 b0 b1).normalize
    acc := acc + r.num
  return acc

partial def handle (j : Json) : Except String Json := do
  let op ← (← j.getObjVal? "op").getStr?
  match op with
  | "ping" => return Json.mkObj [("pong", true)]
  | "dist2" =>
      let a ← point2 j "ax" "ay"
      let b ← point2 j "bx" "by"
      return ratOut (Rat2.ofInt (dist2 a b))
  | "point_seg" =>
      let p ← point2 j "px" "py"
      let a ← point2 j "ax" "ay"
      let b ← point2 j "bx" "by"
      return ratOut (pointSegDist2 p a b)
  | "seg_seg" =>
      let a0 ← point2 j "a0x" "a0y"
      let a1 ← point2 j "a1x" "a1y"
      let b0 ← point2 j "b0x" "b0y"
      let b1 ← point2 j "b1x" "b1y"
      return ratOut (segSegDist2 a0 a1 b0 b1)
  | "separated" =>
      let num ← field j "num"
      let den ← field j "den"
      let c ← field j "clearance"
      return Json.mkObj [("ok", separated num den c)]
  | "norm" =>
      let num ← field j "num"
      let den ← field j "den"
      return ratOut { num, den }
  | "seg_sum" =>
      let n ← field j "n"
      if n < 0 then throw "n < 0"
      if n > 2000000 then throw "n too large"
      let s := segSum n.toNat
      return Json.mkObj [("num", Json.str (toString s)), ("den", "1")]
  | "batch" =>
      let arr ← (← j.getObjVal? "items").getArr?
      let mut out : Array Json := #[]
      for item in arr do
        out := out.push (← handle item)
      return Json.mkObj [("items", Json.arr out)]
  | other => throw s!"unknown op: {other}"

def writeFrame (h : IO.FS.Stream) (s : String) : IO Unit := do
  let b := s.toUTF8
  h.write (pushU32LE b.size)
  h.write b
  h.flush

partial def serve (stdin stdout : IO.FS.Stream) : IO Unit := do
  let hdr ← readExact stdin 4
  if hdr.size < 4 then
    return
  let n := u32le hdr
  if n > 8388608 then
    throw <| IO.userError "frame too large"
  let body ← readExact stdin n
  if body.size < n then
    throw <| IO.userError "truncated payload"
  let text ← IO.ofExcept (bytesToString body)
  let response :=
    match Json.parse text with
    | .error e => (errOut e).compress
    | .ok j =>
        match handle j with
        | .error e => (errOut e).compress
        | .ok out => out.compress
  writeFrame stdout response
  serve stdin stdout

def main : IO UInt32 := do
  serve (← IO.getStdin) (← IO.getStdout)
  return 0
