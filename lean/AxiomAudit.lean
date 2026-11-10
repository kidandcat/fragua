import Lean
import FraguaGeom.Basic

/-
Fail the build if a constant under `FraguaGeom` depends on anything besides
propext, Classical.choice, and Quot.sound. An unfinished proof is rejected
because it depends on sorryAx.
-/

open Lean

private def allowed (n : Name) : Bool :=
  n == ``propext || n == ``Classical.choice || n == ``Quot.sound

elab "#auditFraguaAxioms" : command => do
  let env ← getEnv
  let mut bad : Array MessageData := #[]
  let mut seen : Nat := 0
  let consts := env.constants.map₁.toList ++ env.constants.map₂.toList
  for (name, _) in consts do
    if !(`FraguaGeom).isPrefixOf name then
      continue
    seen := seen + 1
    let axs ← collectAxioms name
    for ax in axs do
      if !allowed ax then
        bad := bad.push m!"{name} depends on {ax}"
  if seen = 0 then
    throwError "audit found no FraguaGeom constants"
  if bad.size ≠ 0 then
    throwError m!"non-standard dependencies:\n{MessageData.joinSep bad.toList "\n"}"
  logInfo m!"audit ok ({seen} constants)"

#auditFraguaAxioms
