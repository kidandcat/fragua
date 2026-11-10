/-
Integer geometry for fragua.

Coordinates are nanometres (`core.Length` is an int64 count of nm). Every
operation below uses Lean's arbitrary-precision `Int`, so a difference or a
product of two endpoint coordinates cannot wrap the way int64 arithmetic
would. The squared distance from a point to a segment is a rational; the
clearance predicate compares that rational with a clearance in nanometres
and never rounds through floating point.

MIT License — see the repository LICENSE.
-/

namespace FraguaGeom

/-- A board point in nanometres. -/
@[ext]
structure Point where
  x : Int
  y : Int
deriving Repr, DecidableEq, BEq

/-- A reduced-or-not rational. Distances use a positive denominator. -/
structure Rat2 where
  num : Int
  den : Int
deriving Repr, DecidableEq, BEq

def Rat2.ofInt (n : Int) : Rat2 := { num := n, den := 1 }

/-- Same rational value. Callers keep denominators positive. -/
def Rat2.valEq (a b : Rat2) : Prop := a.num * b.den = b.num * a.den

def vx (a b : Point) : Int := b.x - a.x
def vy (a b : Point) : Int := b.y - a.y
def len2 (a b : Point) : Int := vx a b * vx a b + vy a b * vy a b

def apx (p a : Point) : Int := p.x - a.x
def apy (p a : Point) : Int := p.y - a.y

/-- Unclamped projection numerator of `p` onto the segment `a → b`. -/
def tnum (p a b : Point) : Int := apx p a * vx a b + apy p a * vy a b

/-- Twice the signed area of triangle `a p b` (the 2D cross product). -/
def crossZ (p a b : Point) : Int := apx p a * vy a b - apy p a * vx a b

/-- Squared Euclidean distance between two points. Always an integer. -/
def dist2 (p q : Point) : Int := apx p q * apx p q + apy p q * apy p q

theorem sq_nonneg_mul (m : Int) : 0 ≤ m * m := by
  simpa [Int.pow_succ, Int.pow_one] using Int.sq_nonneg m

theorem mul_self_eq_zero {m : Int} : m * m = 0 ↔ m = 0 := by
  constructor
  · intro h
    exact (Int.mul_eq_zero.mp h).elim id id
  · intro h
    simp [h]

theorem sum_sq_eq_zero {a b : Int} : a * a + b * b = 0 ↔ a = 0 ∧ b = 0 := by
  constructor
  · intro h
    have ha : 0 ≤ a * a := sq_nonneg_mul a
    have hb : 0 ≤ b * b := sq_nonneg_mul b
    have h0 : a * a = 0 ∧ b * b = 0 := by omega
    exact ⟨mul_self_eq_zero.mp h0.1, mul_self_eq_zero.mp h0.2⟩
  · intro h
    simp [h.1, h.2]

theorem dist2_nonneg (p q : Point) : 0 ≤ dist2 p q := by
  unfold dist2
  have := sq_nonneg_mul (apx p q)
  have := sq_nonneg_mul (apy p q)
  omega

theorem dist2_comm (p q : Point) : dist2 p q = dist2 q p := by
  unfold dist2 apx apy
  grind

theorem dist2_eq_zero_iff (p q : Point) : dist2 p q = 0 ↔ p = q := by
  constructor
  · intro h
    have h0 := sum_sq_eq_zero.mp (by simpa [dist2] using h)
    have hx : p.x = q.x := by
      have : p.x - q.x = 0 := by simpa [apx] using h0.1
      omega
    have hy : p.y = q.y := by
      have : p.y - q.y = 0 := by simpa [apy] using h0.2
      omega
    exact Point.ext hx hy
  · intro h
    cases h
    simp [dist2, apx, apy]

theorem len2_eq_dist2 (a b : Point) : len2 a b = dist2 a b := by
  unfold len2 dist2 vx vy apx apy
  grind

theorem len2_nonneg (a b : Point) : 0 ≤ len2 a b := by
  simpa [len2_eq_dist2] using dist2_nonneg a b

theorem len2_eq_zero_iff (a b : Point) : len2 a b = 0 ↔ a = b := by
  simpa [len2_eq_dist2] using dist2_eq_zero_iff a b

/-- Lagrange identity in the plane: `|ap|²|ab|² - (ap·ab)² = (ap×ab)²`. -/
theorem lagrange (px py bx by_ : Int) :
    (px * px + py * py) * (bx * bx + by_ * by_) - (px * bx + py * by_) * (px * bx + py * by_)
      = (px * by_ - py * bx) * (px * by_ - py * bx) := by
  grind

theorem altNum_eq_cross_sq (p a b : Point) :
    (apx p a * apx p a + apy p a * apy p a) * len2 a b - tnum p a b * tnum p a b
      = crossZ p a b * crossZ p a b := by
  simpa [len2, tnum, crossZ, vx, vy] using
    lagrange (apx p a) (apy p a) (vx a b) (vy a b)

/-- Squared distance from `p` to the closed segment `a–b`, as a rational. -/
def pointSegDist2 (p a b : Point) : Rat2 :=
  let l2 := len2 a b
  let tn := tnum p a b
  if l2 = 0 then
    Rat2.ofInt (dist2 p a)
  else if tn ≤ 0 then
    Rat2.ofInt (dist2 p a)
  else if l2 ≤ tn then
    Rat2.ofInt (dist2 p b)
  else
    { num := (apx p a * apx p a + apy p a * apy p a) * l2 - tn * tn
    , den := l2 }

/-- `p` lies on the closed segment `a–b`, including a degenerate point segment. -/
def onSegment (p a b : Point) : Bool :=
  let l2 := len2 a b
  if l2 = 0 then
    decide (p = a)
  else
    decide (crossZ p a b = 0) && decide (0 ≤ tnum p a b) && decide (tnum p a b ≤ l2)

theorem onSegment_iff (p a b : Point) :
    onSegment p a b = true ↔
      (if len2 a b = 0 then p = a
       else crossZ p a b = 0 ∧ 0 ≤ tnum p a b ∧ tnum p a b ≤ len2 a b) := by
  unfold onSegment
  by_cases h : len2 a b = 0
  · simp [h]
  · simp [h, Bool.and_eq_true]
    constructor
    · intro hiff
      exact ⟨hiff.1.1, hiff.1.2, hiff.2⟩
    · intro hiff
      exact ⟨⟨hiff.1, hiff.2.1⟩, hiff.2.2⟩

theorem pointSegDist2_den_pos (p a b : Point) : 0 < (pointSegDist2 p a b).den := by
  unfold pointSegDist2
  by_cases h0 : len2 a b = 0
  · simp [h0, Rat2.ofInt]
  · by_cases h1 : tnum p a b ≤ 0
    · simp [h0, h1, Rat2.ofInt]
    · by_cases h2 : len2 a b ≤ tnum p a b
      · simp [h0, h1, h2, Rat2.ofInt]
      · simp [h0, h1, h2]
        have hnn := len2_nonneg a b
        omega

theorem pointSegDist2_num_nonneg (p a b : Point) : 0 ≤ (pointSegDist2 p a b).num := by
  unfold pointSegDist2
  by_cases h0 : len2 a b = 0
  · simpa [h0, Rat2.ofInt] using dist2_nonneg p a
  · by_cases h1 : tnum p a b ≤ 0
    · simpa [h0, h1, Rat2.ofInt] using dist2_nonneg p a
    · by_cases h2 : len2 a b ≤ tnum p a b
      · simpa [h0, h1, h2, Rat2.ofInt] using dist2_nonneg p b
      · simp [h0, h1, h2]
        have h := altNum_eq_cross_sq p a b
        have hs : 0 ≤ crossZ p a b * crossZ p a b := sq_nonneg_mul (crossZ p a b)
        omega

/-- A vector orthogonal and parallel to a nonzero vector is zero. -/
theorem parallel_perp_zero (px py bx by_ : Int)
    (hdot : px * bx + py * by_ = 0)
    (hcross : px * by_ - py * bx = 0)
    (hlen : bx * bx + by_ * by_ ≠ 0) : px = 0 ∧ py = 0 := by
  have hlag := lagrange px py bx by_
  have hprod : (px * px + py * py) * (bx * bx + by_ * by_) = 0 := by
    grind
  have hsq : px * px + py * py = 0 := by
    have hmul := Int.mul_eq_zero.mp hprod
    cases hmul with
    | inl h => exact h
    | inr h => exact absurd h hlen
  exact sum_sq_eq_zero.mp hsq

/-- If the projection of `ap` onto `ab` is `ab` itself and the cross product
vanishes, the two vectors are equal. -/
theorem coincide_of_dot_cross (px py bx by_ : Int)
    (hcross : px * by_ - py * bx = 0)
    (hdot : px * bx + py * by_ = bx * bx + by_ * by_)
    (hlen : bx * bx + by_ * by_ ≠ 0) : px = bx ∧ py = by_ := by
  have hlag := lagrange px py bx by_
  rw [hcross, hdot] at hlag
  have hfac :
      (bx * bx + by_ * by_) * ((px * px + py * py) - (bx * bx + by_ * by_)) = 0 := by
    grind
  have happ : px * px + py * py = bx * bx + by_ * by_ := by
    cases Int.mul_eq_zero.mp hfac with
    | inl h => exact absurd h hlen
    | inr h => omega
  have hdx : (px - bx) * (px - bx) + (py - by_) * (py - by_) = 0 := by
    grind
  have h0 := sum_sq_eq_zero.mp hdx
  constructor <;> omega

theorem tnum_right (a b : Point) : tnum b a b = len2 a b := by
  unfold tnum len2 apx apy vx vy
  grind

theorem cross_right (a b : Point) : crossZ b a b = 0 := by
  unfold crossZ apx apy vx vy
  grind

theorem pointSegDist2_eq_zero_iff_onSegment (p a b : Point) :
    (pointSegDist2 p a b).num = 0 ↔ onSegment p a b = true := by
  rw [onSegment_iff]
  constructor
  · intro h
    by_cases h0 : len2 a b = 0
    · have hp : p = a := by
        have : dist2 p a = 0 := by simpa [pointSegDist2, h0, Rat2.ofInt] using h
        exact (dist2_eq_zero_iff p a).mp this
      simp [h0, hp]
    · by_cases h1 : tnum p a b ≤ 0
      · have hp : p = a := by
          have : dist2 p a = 0 := by simpa [pointSegDist2, h0, h1, Rat2.ofInt] using h
          exact (dist2_eq_zero_iff p a).mp this
        have ht : tnum p a b = 0 := by simp [tnum, apx, apy, hp]
        have hc : crossZ p a b = 0 := by simp [crossZ, apx, apy, hp]
        simp [h0, ht, hc, len2_nonneg a b]
      · by_cases h2 : len2 a b ≤ tnum p a b
        · have hp : p = b := by
            have : dist2 p b = 0 := by simpa [pointSegDist2, h0, h1, h2, Rat2.ofInt] using h
            exact (dist2_eq_zero_iff p b).mp this
          have ht : tnum p a b = len2 a b := by simpa [hp] using tnum_right a b
          have hc : crossZ p a b = 0 := by simpa [hp] using cross_right a b
          simp [h0, ht, hc, len2_nonneg a b]
        · have hcross : crossZ p a b = 0 := by
            have hnum : (apx p a * apx p a + apy p a * apy p a) * len2 a b
                - tnum p a b * tnum p a b = 0 := by
              simpa [pointSegDist2, h0, h1, h2] using h
            have hsq : crossZ p a b * crossZ p a b = 0 := by
              have hlt := altNum_eq_cross_sq p a b
              omega
            exact mul_self_eq_zero.mp hsq
          have htn : 0 ≤ tnum p a b := by omega
          have hle : tnum p a b ≤ len2 a b := by omega
          simp [h0, hcross, htn, hle]
  · intro h
    by_cases h0 : len2 a b = 0
    · have hp : p = a := by simpa [h0] using h
      simp [pointSegDist2, h0, Rat2.ofInt, (dist2_eq_zero_iff p a).mpr hp]
    · simp [h0] at h
      have hc : crossZ p a b = 0 := h.1
      have ht0 : 0 ≤ tnum p a b := h.2.1
      have hle : tnum p a b ≤ len2 a b := h.2.2
      by_cases h1 : tnum p a b ≤ 0
      · have ht : tnum p a b = 0 := by omega
        have hzero := parallel_perp_zero (apx p a) (apy p a) (vx a b) (vy a b)
          (by simpa [tnum] using ht) (by simpa [crossZ] using hc)
          (by simpa [len2] using h0)
        have hp : p = a := by
          apply Point.ext
          · have : p.x - a.x = 0 := by simpa [apx] using hzero.1
            omega
          · have : p.y - a.y = 0 := by simpa [apy] using hzero.2
            omega
        simp [pointSegDist2, h0, h1, Rat2.ofInt, (dist2_eq_zero_iff p a).mpr hp]
      · by_cases h2 : len2 a b ≤ tnum p a b
        · have ht : tnum p a b = len2 a b := by omega
          have hcoin := coincide_of_dot_cross (apx p a) (apy p a) (vx a b) (vy a b)
            (by simpa [crossZ] using hc)
            (by simpa [tnum, len2] using ht)
            (by simpa [len2] using h0)
          have hp : p = b := by
            apply Point.ext
            · have : p.x - a.x = b.x - a.x := by simpa [apx, vx] using hcoin.1
              omega
            · have : p.y - a.y = b.y - a.y := by simpa [apy, vy] using hcoin.2
              omega
          simp [pointSegDist2, h0, h1, h2, Rat2.ofInt, (dist2_eq_zero_iff p b).mpr hp]
        · have hsq : crossZ p a b * crossZ p a b = 0 := by simp [hc]
          simp [pointSegDist2, h0, h1, h2, altNum_eq_cross_sq, hsq]

theorem dist2_b_sub (p a b : Point) :
    dist2 p b - dist2 p a = len2 a b - 2 * tnum p a b := by
  unfold dist2 len2 tnum apx apy vx vy
  grind

theorem pointSegDist2_le_left (p a b : Point) :
    (pointSegDist2 p a b).num ≤ dist2 p a * (pointSegDist2 p a b).den := by
  by_cases h0 : len2 a b = 0
  · simp [pointSegDist2, h0, Rat2.ofInt]
  · by_cases h1 : tnum p a b ≤ 0
    · simp [pointSegDist2, h0, h1, Rat2.ofInt]
    · by_cases h2 : len2 a b ≤ tnum p a b
      · simp [pointSegDist2, h0, h1, h2, Rat2.ofInt]
        have hid := dist2_b_sub p a b
        have hnn := len2_nonneg a b
        omega
      · have hs : 0 ≤ tnum p a b * tnum p a b := sq_nonneg_mul (tnum p a b)
        have hineq :
            (apx p a * apx p a + apy p a * apy p a) * len2 a b - tnum p a b * tnum p a b
              ≤ (apx p a * apx p a + apy p a * apy p a) * len2 a b := by
          omega
        simpa [pointSegDist2, h0, h1, h2, dist2] using hineq

theorem pointSegDist2_le_right (p a b : Point) :
    (pointSegDist2 p a b).num ≤ dist2 p b * (pointSegDist2 p a b).den := by
  by_cases h0 : len2 a b = 0
  · have hab : a = b := (len2_eq_zero_iff a b).mp h0
    have hbb : len2 b b = 0 := (len2_eq_zero_iff b b).mpr rfl
    simp [pointSegDist2, hab, hbb, Rat2.ofInt]
  · by_cases h1 : tnum p a b ≤ 0
    · simp [pointSegDist2, h0, h1, Rat2.ofInt]
      have hid := dist2_b_sub p a b
      have hnn := len2_nonneg a b
      omega
    · by_cases h2 : len2 a b ≤ tnum p a b
      · simp [pointSegDist2, h0, h1, h2, Rat2.ofInt]
      · have hdiff :
            dist2 p b * len2 a b -
              ((apx p a * apx p a + apy p a * apy p a) * len2 a b - tnum p a b * tnum p a b)
            = (len2 a b - tnum p a b) * (len2 a b - tnum p a b) := by
          unfold dist2 len2 tnum apx apy vx vy
          grind
        have hs : 0 ≤ (len2 a b - tnum p a b) * (len2 a b - tnum p a b) :=
          sq_nonneg_mul (len2 a b - tnum p a b)
        simp [pointSegDist2, h0, h1, h2]
        omega

theorem pointSegDist2_degenerate (p a : Point) :
    pointSegDist2 p a a = Rat2.ofInt (dist2 p a) := by
  have h : len2 a a = 0 := (len2_eq_zero_iff a a).mpr rfl
  simp [pointSegDist2, h]

theorem pointSegDist2_left_endpoint (a b : Point) : (pointSegDist2 a a b).num = 0 := by
  have ht : tnum a a b = 0 := by simp [tnum, apx, apy]
  have hd : dist2 a a = 0 := (dist2_eq_zero_iff a a).mpr rfl
  by_cases h0 : len2 a b = 0
  · simp [pointSegDist2, h0, Rat2.ofInt, hd]
  · have h1 : tnum a a b ≤ 0 := by simp [ht]
    simp [pointSegDist2, h0, h1, Rat2.ofInt, hd]

theorem pointSegDist2_right_endpoint (a b : Point) : (pointSegDist2 b a b).num = 0 := by
  have ht : tnum b a b = len2 a b := tnum_right a b
  have hd : dist2 b b = 0 := (dist2_eq_zero_iff b b).mpr rfl
  by_cases h0 : len2 a b = 0
  · have hab : a = b := (len2_eq_zero_iff a b).mp h0
    have hbb : len2 b b = 0 := (len2_eq_zero_iff b b).mpr rfl
    simp [pointSegDist2, hab, hbb, Rat2.ofInt, hd]
  · have h1 : ¬ tnum b a b ≤ 0 := by
      rw [ht]
      have hnn := len2_nonneg a b
      omega
    have h2 : len2 a b ≤ tnum b a b := by simp [ht]
    simp [pointSegDist2, h0, h1, h2, Rat2.ofInt, hd]

/-- Strict opposite signs. Zero on either side is not a proper crossing. -/
def oppositeStrict (x y : Int) : Bool :=
  (decide (0 < x) && decide (y < 0)) || (decide (x < 0) && decide (0 < y))

def orient (a b c : Point) : Int :=
  (b.x - a.x) * (c.y - a.y) - (b.y - a.y) * (c.x - a.x)

/-- Proper intersection of the open segments: endpoints lie on opposite
sides of the other line, both ways. Collinear overlap is not included;
that case is a zero distance through `onSegment`. -/
def properCross (a0 a1 b0 b1 : Point) : Bool :=
  oppositeStrict (orient a0 a1 b0) (orient a0 a1 b1) &&
    oppositeStrict (orient b0 b1 a0) (orient b0 b1 a1)

theorem properCross_symm (a0 a1 b0 b1 : Point) :
    properCross a0 a1 b0 b1 = properCross b0 b1 a0 a1 := by
  unfold properCross
  grind

def minRat (a b : Rat2) : Rat2 :=
  if a.num * b.den ≤ b.num * a.den then a else b

theorem minRat_comm_valEq (a b : Rat2) : Rat2.valEq (minRat a b) (minRat b a) := by
  unfold minRat Rat2.valEq
  by_cases h1 : a.num * b.den ≤ b.num * a.den
  · by_cases h2 : b.num * a.den ≤ a.num * b.den
    · simp [h1, h2]
      omega
    · simp [h1, h2]
  · by_cases h2 : b.num * a.den ≤ a.num * b.den
    · simp [h1, h2]
    · have : a.num * b.den ≤ b.num * a.den := by omega
      exact absurd this h1

theorem minRat_num_nonneg (a b : Rat2) (ha : 0 ≤ a.num) (hb : 0 ≤ b.num) :
    0 ≤ (minRat a b).num := by
  unfold minRat
  by_cases h : a.num * b.den ≤ b.num * a.den
  · simpa [h] using ha
  · simpa [h] using hb

theorem minRat_den_pos (a b : Rat2) (ha : 0 < a.den) (hb : 0 < b.den) :
    0 < (minRat a b).den := by
  unfold minRat
  by_cases h : a.num * b.den ≤ b.num * a.den
  · simpa [h] using ha
  · simpa [h] using hb

theorem minRat_num_eq_zero (a b : Rat2)
    (ha : 0 < a.den) (hb : 0 < b.den) (hna : 0 ≤ a.num) (hnb : 0 ≤ b.num) :
    (minRat a b).num = 0 ↔ a.num = 0 ∨ b.num = 0 := by
  unfold minRat
  constructor
  · intro h
    by_cases hle : a.num * b.den ≤ b.num * a.den
    · exact Or.inl (by simpa [hle] using h)
    · exact Or.inr (by simpa [hle] using h)
  · intro h
    by_cases hle : a.num * b.den ≤ b.num * a.den
    · simp [hle]
      cases h with
      | inl hz => exact hz
      | inr hz =>
          have hprod : a.num * b.den ≤ 0 := by simpa [hz] using hle
          have hnn : 0 ≤ a.num * b.den := Int.mul_nonneg hna (Int.le_of_lt hb)
          have hzero : a.num * b.den = 0 := by omega
          cases Int.mul_eq_zero.mp hzero with
          | inl ha0 => exact ha0
          | inr hd => omega
    · simp [hle]
      cases h with
      | inl hz =>
          have : a.num * b.den ≤ b.num * a.den := by
            have hnn : 0 ≤ b.num * a.den := Int.mul_nonneg hnb (Int.le_of_lt ha)
            simpa [hz, Int.zero_mul] using hnn
          exact absurd this hle
      | inr hz => exact hz

/-- Squared distance between two closed segments.
A proper crossing is `0`; otherwise the distance is the minimum of the four
endpoint-to-segment distances. In the plane that minimum is the distance:
an interior-interior closest pair only happens for parallel overlapping
projections, and then an endpoint realises it, or for a crossing, which is
the `0` branch. -/
def segSegDist2 (a0 a1 b0 b1 : Point) : Rat2 :=
  if properCross a0 a1 b0 b1 then
    { num := 0, den := 1 }
  else
    minRat (minRat (pointSegDist2 a0 b0 b1) (pointSegDist2 a1 b0 b1))
      (minRat (pointSegDist2 b0 a0 a1) (pointSegDist2 b1 a0 a1))

theorem segSegDist2_cross (a0 a1 b0 b1 : Point) (h : properCross a0 a1 b0 b1 = true) :
    segSegDist2 a0 a1 b0 b1 = { num := 0, den := 1 } := by
  simp [segSegDist2, h]

theorem segSegDist2_spec (a0 a1 b0 b1 : Point) (h : properCross a0 a1 b0 b1 = false) :
    segSegDist2 a0 a1 b0 b1 =
      minRat (minRat (pointSegDist2 a0 b0 b1) (pointSegDist2 a1 b0 b1))
        (minRat (pointSegDist2 b0 a0 a1) (pointSegDist2 b1 a0 a1)) := by
  simp [segSegDist2, h]

theorem segSegDist2_den_pos (a0 a1 b0 b1 : Point) : 0 < (segSegDist2 a0 a1 b0 b1).den := by
  by_cases h : properCross a0 a1 b0 b1 = true
  · simp [segSegDist2, h]
  · have hf : properCross a0 a1 b0 b1 = false := by simpa [Bool.not_eq_true] using h
    simp [segSegDist2, hf]
    exact minRat_den_pos _ _ (minRat_den_pos _ _ (pointSegDist2_den_pos _ _ _) (pointSegDist2_den_pos _ _ _))
      (minRat_den_pos _ _ (pointSegDist2_den_pos _ _ _) (pointSegDist2_den_pos _ _ _))

theorem segSegDist2_num_nonneg (a0 a1 b0 b1 : Point) : 0 ≤ (segSegDist2 a0 a1 b0 b1).num := by
  by_cases h : properCross a0 a1 b0 b1 = true
  · simp [segSegDist2, h]
  · have hf : properCross a0 a1 b0 b1 = false := by simpa [Bool.not_eq_true] using h
    simp [segSegDist2, hf]
    exact minRat_num_nonneg _ _
      (minRat_num_nonneg _ _ (pointSegDist2_num_nonneg _ _ _) (pointSegDist2_num_nonneg _ _ _))
      (minRat_num_nonneg _ _ (pointSegDist2_num_nonneg _ _ _) (pointSegDist2_num_nonneg _ _ _))

theorem segSegDist2_symm (a0 a1 b0 b1 : Point) :
    Rat2.valEq (segSegDist2 a0 a1 b0 b1) (segSegDist2 b0 b1 a0 a1) := by
  unfold segSegDist2
  have hs := properCross_symm a0 a1 b0 b1
  by_cases h : properCross a0 a1 b0 b1 = true
  · have h' : properCross b0 b1 a0 a1 = true := by simpa [hs] using h
    simp [h, h', Rat2.valEq]
  · have h' : properCross b0 b1 a0 a1 = false := by
      simpa [hs, Bool.not_eq_true] using h
    simp [h, h']
    exact minRat_comm_valEq
      (minRat (pointSegDist2 a0 b0 b1) (pointSegDist2 a1 b0 b1))
      (minRat (pointSegDist2 b0 a0 a1) (pointSegDist2 b1 a0 a1))

def touches (a0 a1 b0 b1 : Point) : Bool :=
  properCross a0 a1 b0 b1 ||
    onSegment a0 b0 b1 || onSegment a1 b0 b1 ||
    onSegment b0 a0 a1 || onSegment b1 a0 a1

theorem or_left4 {a b c d : Prop} :
    a ∨ b ∨ c ∨ d → ((a ∨ b) ∨ c) ∨ d := by
  intro h
  cases h with
  | inl ha => exact Or.inl (Or.inl (Or.inl ha))
  | inr h =>
      cases h with
      | inl hb => exact Or.inl (Or.inl (Or.inr hb))
      | inr h =>
          cases h with
          | inl hc => exact Or.inl (Or.inr hc)
          | inr hd => exact Or.inr hd

theorem or_right4 {a b c d : Prop} :
    ((a ∨ b) ∨ c) ∨ d → a ∨ b ∨ c ∨ d := by
  intro h
  cases h with
  | inl h =>
      cases h with
      | inl h =>
          cases h with
          | inl ha => exact Or.inl ha
          | inr hb => exact Or.inr (Or.inl hb)
      | inr hc => exact Or.inr (Or.inr (Or.inl hc))
  | inr hd => exact Or.inr (Or.inr (Or.inr hd))

theorem segSegDist2_eq_zero_iff_touches (a0 a1 b0 b1 : Point) :
    (segSegDist2 a0 a1 b0 b1).num = 0 ↔ touches a0 a1 b0 b1 = true := by
  by_cases h : properCross a0 a1 b0 b1 = true
  · constructor
    · intro _
      simp [touches, h]
    · intro _
      simpa using congrArg Rat2.num (segSegDist2_cross a0 a1 b0 b1 h)
  · have hf : properCross a0 a1 b0 b1 = false := by simpa [Bool.not_eq_true] using h
    have hspec := segSegDist2_spec a0 a1 b0 b1 hf
    -- Four endpoint distances, all non-negative with positive denominators.
    let w := pointSegDist2 a0 b0 b1
    let x := pointSegDist2 a1 b0 b1
    let y := pointSegDist2 b0 a0 a1
    let z := pointSegDist2 b1 a0 a1
    have hw : 0 ≤ w.num := pointSegDist2_num_nonneg a0 b0 b1
    have hx : 0 ≤ x.num := pointSegDist2_num_nonneg a1 b0 b1
    have hy : 0 ≤ y.num := pointSegDist2_num_nonneg b0 a0 a1
    have hz : 0 ≤ z.num := pointSegDist2_num_nonneg b1 a0 a1
    have hwd : 0 < w.den := pointSegDist2_den_pos a0 b0 b1
    have hxd : 0 < x.den := pointSegDist2_den_pos a1 b0 b1
    have hyd : 0 < y.den := pointSegDist2_den_pos b0 a0 a1
    have hzd : 0 < z.den := pointSegDist2_den_pos b1 a0 a1
    have hwx := minRat_num_eq_zero w x hwd hxd hw hx
    have hyz := minRat_num_eq_zero y z hyd hzd hy hz
    have hden1 := minRat_den_pos w x hwd hxd
    have hden2 := minRat_den_pos y z hyd hzd
    have hnum1 := minRat_num_nonneg w x hw hx
    have hnum2 := minRat_num_nonneg y z hy hz
    have htop := minRat_num_eq_zero (minRat w x) (minRat y z) hden1 hden2 hnum1 hnum2
    have hmin : (minRat (minRat w x) (minRat y z)).num = 0 ↔
        w.num = 0 ∨ x.num = 0 ∨ y.num = 0 ∨ z.num = 0 := by
      constructor
      · intro hz0
        cases htop.mp hz0 with
        | inl hpair =>
            cases hwx.mp hpair with
            | inl hw0 => exact Or.inl hw0
            | inr hx0 => exact Or.inr (Or.inl hx0)
        | inr hpair =>
            cases hyz.mp hpair with
            | inl hy0 => exact Or.inr (Or.inr (Or.inl hy0))
            | inr hz0 => exact Or.inr (Or.inr (Or.inr hz0))
      · intro hzero
        apply htop.mpr
        cases hzero with
        | inl hw0 => exact Or.inl (hwx.mpr (Or.inl hw0))
        | inr hrest =>
            cases hrest with
            | inl hx0 => exact Or.inl (hwx.mpr (Or.inr hx0))
            | inr hrest =>
                cases hrest with
                | inl hy0 => exact Or.inr (hyz.mpr (Or.inl hy0))
                | inr hz0 => exact Or.inr (hyz.mpr (Or.inr hz0))
    have hnum : (segSegDist2 a0 a1 b0 b1).num = (minRat (minRat w x) (minRat y z)).num := by
      simpa [w, x, y, z] using congrArg Rat2.num hspec
    constructor
    · intro hznum
      have hzero : w.num = 0 ∨ x.num = 0 ∨ y.num = 0 ∨ z.num = 0 := by
        have : (minRat (minRat w x) (minRat y z)).num = 0 := by simpa [hnum] using hznum
        exact hmin.mp this
      have htouch : onSegment a0 b0 b1 = true ∨ onSegment a1 b0 b1 = true ∨
          onSegment b0 a0 a1 = true ∨ onSegment b1 a0 a1 = true := by
        simpa [w, x, y, z, pointSegDist2_eq_zero_iff_onSegment] using hzero
      simp [hf, Bool.or_eq_true, touches]
      exact or_left4 htouch
    · intro ht
      have htouch : onSegment a0 b0 b1 = true ∨ onSegment a1 b0 b1 = true ∨
          onSegment b0 a0 a1 = true ∨ onSegment b1 a0 a1 = true := by
        simp [hf, Bool.or_eq_true, touches] at ht
        exact or_right4 ht
      have hzero : w.num = 0 ∨ x.num = 0 ∨ y.num = 0 ∨ z.num = 0 := by
        simpa [w, x, y, z, pointSegDist2_eq_zero_iff_onSegment] using htouch
      have : (minRat (minRat w x) (minRat y z)).num = 0 := hmin.mpr hzero
      simpa [hnum] using this

/-- `num/den` is at least `clearance` nanometres, squared.
A non-positive denominator or a negative numerator is not a squared distance
and answers `false`. A negative clearance is the same predicate as clearance 0
(every real distance qualifies). -/
def separated (num den clearance : Int) : Bool :=
  if den ≤ 0 ∨ num < 0 then
    false
  else
    let c := if clearance < 0 then (0 : Int) else clearance
    decide (num ≥ c * c * den)

theorem separated_invalid (num den clearance : Int) (h : den ≤ 0 ∨ num < 0) :
    separated num den clearance = false := by
  simp [separated, h]

theorem separated_spec (num den c : Int) (hd : 0 < den) (hn : 0 ≤ num) (hc : 0 ≤ c) :
    separated num den c = true ↔ num ≥ c * c * den := by
  have hden : ¬ den ≤ 0 := by omega
  have hnum : ¬ num < 0 := by omega
  have hc' : ¬ c < 0 := by omega
  simp [separated, hden, hnum, hc']

theorem separated_neg_clearance (num den c : Int) (hc : c < 0) :
    separated num den c = separated num den 0 := by
  simp [separated, hc]

theorem sq_mono_nonneg {c1 c2 : Int} (h1 : 0 ≤ c1) (h12 : c1 ≤ c2) : c1 * c1 ≤ c2 * c2 := by
  have hdiff : c2 * c2 - c1 * c1 = (c2 - c1) * (c2 + c1) := by grind
  have hnn : 0 ≤ (c2 - c1) * (c2 + c1) := by
    have ha : 0 ≤ c2 - c1 := by omega
    have hb : 0 ≤ c2 + c1 := by omega
    exact Int.mul_nonneg ha hb
  omega

theorem separated_mono (num den c1 c2 : Int)
    (hd : 0 < den) (hn : 0 ≤ num) (hc1 : 0 ≤ c1) (hc12 : c1 ≤ c2)
    (h : separated num den c2 = true) : separated num den c1 = true := by
  have hc2 : 0 ≤ c2 := by omega
  have h2 := (separated_spec num den c2 hd hn hc2).mp h
  have hsq := sq_mono_nonneg hc1 hc12
  have hden : 0 ≤ den := by omega
  have hmul : c1 * c1 * den ≤ c2 * c2 * den := by
    have := Int.mul_le_mul_of_nonneg_right hsq hden
    simpa [Int.mul_assoc, Int.mul_left_comm, Int.mul_comm] using this
  have : num ≥ c1 * c1 * den := by omega
  exact (separated_spec num den c1 hd hn hc1).mpr this

theorem separated_dist2 (p q : Point) (c : Int) (hc : 0 ≤ c) :
    separated (dist2 p q) 1 c = true ↔ dist2 p q ≥ c * c := by
  have h := separated_spec (dist2 p q) 1 c (by omega) (dist2_nonneg p q) hc
  simpa using h

/-- Canonical fraction: positive denominator, gcd 1. `0/0` stays a sentinel. -/
def Rat2.normalize (r : Rat2) : Rat2 :=
  let num := if r.den < 0 then -r.num else r.num
  let den := if r.den < 0 then -r.den else r.den
  if den = 0 then
    { num := 0, den := 0 }
  else
    let g : Int := Int.gcd num den
    { num := num / g, den := den / g }

/-! Executable checks. These evaluate the same functions the server exports. -/

#guard dist2 ⟨0, 0⟩ ⟨3, 4⟩ = 25
#guard dist2 ⟨3, 4⟩ ⟨0, 0⟩ = 25
#guard (pointSegDist2 ⟨1, 1⟩ ⟨0, 0⟩ ⟨4, 0⟩).normalize = { num := 1, den := 1 }
#guard (pointSegDist2 ⟨0, 1⟩ ⟨0, 0⟩ ⟨1, 1⟩).normalize = { num := 1, den := 2 }
#guard (pointSegDist2 ⟨0, 0⟩ ⟨0, 0⟩ ⟨5, 0⟩).num = 0
#guard (pointSegDist2 ⟨5, 0⟩ ⟨0, 0⟩ ⟨5, 0⟩).num = 0
#guard (pointSegDist2 ⟨9, 0⟩ ⟨0, 0⟩ ⟨5, 0⟩).normalize = { num := 16, den := 1 }
#guard (segSegDist2 ⟨0, 0⟩ ⟨2, 0⟩ ⟨1, -1⟩ ⟨1, 1⟩).num = 0
#guard (segSegDist2 ⟨0, 0⟩ ⟨2, 0⟩ ⟨0, 2⟩ ⟨2, 2⟩).normalize = { num := 4, den := 1 }
#guard (segSegDist2 ⟨0, 0⟩ ⟨0, 0⟩ ⟨3, 4⟩ ⟨3, 4⟩).normalize = { num := 25, den := 1 }
#guard separated 25 1 5 = true
#guard separated 25 1 6 = false
#guard separated 1 2 1 = false
#guard separated 1 2 0 = true
#guard separated 1 2 (-3) = separated 1 2 0
#guard ({ num := 2, den := 4 : Rat2 }).normalize = { num := 1, den := 2 }
#guard ({ num := 0, den := 5 : Rat2 }).normalize = { num := 0, den := 1 }
#guard ({ num := 2, den := -4 : Rat2 }).normalize = { num := -1, den := 2 }
#guard ({ num := -2, den := -4 : Rat2 }).normalize = { num := 1, den := 2 }

end FraguaGeom
