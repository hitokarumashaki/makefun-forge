// The seeded scenario's identity, ported from the kit's wire.ts (the one fixed
// seed every check derives its data from). Pure port of the PRNG and name lists;
// contains no check logic. Arithmetic is uint64 (wraps mod 2^64, like BigInt
// masked to 64 bits in the TypeScript original).

package main

import "strconv"

const scenarioSeed uint64 = 0x6a09e667f3bcc909
const u64Mask uint64 = 0xffffffffffffffff

func fnv1a64(text string) uint64 {
	hash := uint64(0xcbf29ce484222325)
	for i := 0; i < len(text); i++ {
		hash ^= uint64(text[i])
		hash = (hash * 0x100000001b3) & u64Mask
	}
	return hash
}

func mix64(value uint64) uint64 {
	z := (value + 0x9e3779b97f4a7c15) & u64Mask
	z = ((z ^ (z >> 30)) * 0xbf58476d1ce4e5b9) & u64Mask
	z = ((z ^ (z >> 27)) * 0x94d049bb133111eb) & u64Mask
	return (z ^ (z >> 31)) & u64Mask
}

func scenarioSeedFor(label string) uint64 {
	return mix64(scenarioSeed ^ fnv1a64(label))
}

type rng struct {
	state uint64
}

func newRng(seed uint64) *rng {
	masked := seed & u64Mask
	if masked == 0 {
		masked = scenarioSeed
	}
	return &rng{state: masked}
}

func (r *rng) nextU64() uint64 {
	x := r.state
	x = (x ^ (x >> 12)) & u64Mask
	x = (x ^ (x << 25)) & u64Mask
	x = (x ^ (x >> 27)) & u64Mask
	r.state = x
	return (x * 0x2545f4914f6cdd1d) & u64Mask
}

func (r *rng) nextU32() uint32 {
	return uint32(r.nextU64() >> 32)
}

func (r *rng) nextInt(maxExclusive int) int {
	if maxExclusive == 1 {
		return 0
	}
	// 2^32 does not fit in a uint32, so the range math runs in uint64.
	const rngRange uint64 = 4294967296
	limit := rngRange - (rngRange % uint64(maxExclusive))
	for {
		value := uint64(r.nextU32())
		if value < limit {
			return int(value % uint64(maxExclusive))
		}
	}
}

func (r *rng) pick(items []string) string {
	return items[r.nextInt(len(items))]
}

var firstNames = []string{
	"ada", "bram", "cora", "dmitri", "elena", "felix", "greta", "hugo", "iris", "jonas", "kira", "lars",
	"mira", "nico", "olga", "pavel", "quinn", "rosa", "sven", "tess", "ulla", "vito", "wren", "yara", "zane",
}

var lastNames = []string{
	"anders", "bauer", "costa", "dahl", "eriksen", "fischer", "garcia", "holm", "iversen", "jansen", "keller",
	"lind", "moreau", "novak", "olsen", "peters", "quist", "rossi", "sato", "turi", "ulrich", "vega", "weber",
	"xu", "yilmaz", "zimmer",
}

type ScenarioUser struct {
	ID    int
	Login string
	Name  string
	Email string
}

func generateUsers(count int) []ScenarioUser {
	r := newRng(scenarioSeedFor("users"))
	used := map[string]bool{}
	users := make([]ScenarioUser, 0, count)
	for i := 0; i < count; i++ {
		first := r.pick(firstNames)
		last := r.pick(lastNames)
		base := ""
		for _, ch := range first + last {
			if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' {
				base += string(ch)
			}
		}
		login := base
		suffix := 2
		for used[login] {
			login = base + "-" + strconv.Itoa(suffix)
			suffix++
		}
		used[login] = true
		users = append(users, ScenarioUser{ID: 1000 + i, Login: login, Name: first + " " + last, Email: login + "@example.invalid"})
	}
	return users
}

var users = generateUsers(12)

// VIEWER is the login the trial's bearer token owns (the scenario's viewer).
var VIEWER = users[0]

// STRANGER is another seeded login, with no rights on the viewer's repositories.
var STRANGER = users[2]
