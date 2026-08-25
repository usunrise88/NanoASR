package itn

import "strings"

// Russian number words, in every case and gender the recogniser emits.
//
// The lists are deliberately explicit rather than generated from a stemmer:
// "три" and "трёх" and "тремя" are the same number, and a rule that guessed at
// endings would also turn "тристапятьдесят" into something when the speaker
// said a word that merely starts the same way.
var ruUnits = map[string]int64{
	"ноль": 0, "нуль": 0,
	"один": 1, "одна": 1, "одно": 1, "одного": 1, "одной": 1, "одним": 1,
	"два": 2, "две": 2, "двух": 2, "двум": 2, "двумя": 2,
	"три": 3, "трёх": 3, "трех": 3, "трём": 3, "трем": 3, "тремя": 3,
	"четыре": 4, "четырёх": 4, "четырех": 4, "четырём": 4, "четырем": 4, "четырьмя": 4,
	"пять": 5, "пяти": 5, "пятью": 5,
	"шесть": 6, "шести": 6, "шестью": 6,
	"семь": 7, "семи": 7, "семью": 7,
	"восемь": 8, "восьми": 8, "восемью": 8, "восьмью": 8,
	"девять": 9, "девяти": 9, "девятью": 9,
	"десять": 10, "десяти": 10, "десятью": 10,
	"одиннадцать": 11, "одиннадцати": 11,
	"двенадцать": 12, "двенадцати": 12,
	"тринадцать": 13, "тринадцати": 13,
	"четырнадцать": 14, "четырнадцати": 14,
	"пятнадцать": 15, "пятнадцати": 15,
	"шестнадцать": 16, "шестнадцати": 16,
	"семнадцать": 17, "семнадцати": 17,
	"восемнадцать": 18, "восемнадцати": 18,
	"девятнадцать": 19, "девятнадцати": 19,
}

var ruTens = map[string]int64{
	"двадцать": 20, "двадцати": 20,
	"тридцать": 30, "тридцати": 30,
	"сорок": 40, "сорока": 40,
	"пятьдесят": 50, "пятидесяти": 50,
	"шестьдесят": 60, "шестидесяти": 60,
	"семьдесят": 70, "семидесяти": 70,
	"восемьдесят": 80, "восьмидесяти": 80,
	"девяносто": 90, "девяноста": 90,
}

var ruHundreds = map[string]int64{
	"сто": 100, "ста": 100,
	"двести": 200, "двухсот": 200, "двумстам": 200,
	"триста": 300, "трёхсот": 300, "трехсот": 300,
	"четыреста": 400, "четырёхсот": 400, "четырехсот": 400,
	"пятьсот": 500, "пятисот": 500,
	"шестьсот": 600, "шестисот": 600,
	"семьсот": 700, "семисот": 700,
	"восемьсот": 800, "восьмисот": 800,
	"девятьсот": 900, "девятисот": 900,
}

// ruScales are multipliers. Order matters when parsing: a scale closes the
// group before it and multiplies it.
var ruScales = map[string]int64{
	"тысяча": 1_000, "тысячи": 1_000, "тысяч": 1_000, "тысячу": 1_000, "тысячей": 1_000,
	"миллион": 1_000_000, "миллиона": 1_000_000, "миллионов": 1_000_000,
	"миллиард": 1_000_000_000, "миллиарда": 1_000_000_000, "миллиардов": 1_000_000_000,
}

// number is a parsed run of number words.
type number struct {
	value int64
	count int // how many tokens it consumed
}

// parseNumber reads the longest number that starts at in[0].
//
// Russian says compound numbers by addition within a group and multiplication
// across scales: "две тысячи двадцать пять" is (2 × 1000) + 20 + 5. The group
// accumulator holds the current group, total holds the closed ones.
func parseNumber(in []Token) (number, bool) {
	var total, group int64
	consumed, sawAny, sawGroup := 0, false, false

	for i := 0; i < len(in); i++ {
		if i > 0 && !joins(in[i-1], in[i]) {
			break
		}
		// The default model punctuates, so number words arrive with commas
		// attached. Matching the raw token would make ITN fail on exactly the
		// transcripts it is most wanted for.
		w, mark := trimPunct(in[i].Lower)

		switch {
		case ruHundreds[w] != 0:
			group += ruHundreds[w]
		case ruTens[w] != 0:
			group += ruTens[w]
		case isUnit(w):
			// A unit after a unit is two numbers, not one: "пять шесть" is a
			// digit sequence, and merging it into 11 would invent a value.
			if sawGroup && group%10 != 0 {
				return number{}, false
			}
			group += ruUnits[w]
		case ruScales[w] != 0:
			scale := ruScales[w]
			if group == 0 {
				// "тысяча рублей" means one thousand, not zero thousand.
				group = 1
			}
			total += group * scale
			group = 0
			// A scale closes its group rather than extending it, so the next
			// unit starts a fresh one.
			sawGroup = false
			sawAny = true
			consumed = i + 1
			if mark != "" {
				return closeNumber(total, group, consumed, sawAny)
			}
			continue
		default:
			// Not a number word: the run ends here.
			return closeNumber(total, group, consumed, sawAny)
		}
		sawGroup = true
		sawAny = true
		consumed = i + 1
		// A mark ends the run: "двадцать, пять" is two thoughts, and the comma
		// is the speaker's own boundary rather than punctuation inside a
		// number. The word carrying it is still part of what came before.
		if mark != "" {
			break
		}
	}
	return closeNumber(total, group, consumed, sawAny)
}

func closeNumber(total, group int64, consumed int, sawAny bool) (number, bool) {
	if !sawAny || consumed == 0 {
		return number{}, false
	}
	return number{value: total + group, count: consumed}, true
}

func isUnit(w string) bool {
	_, ok := ruUnits[w]
	return ok
}

// joins reports whether two adjacent tokens may belong to one rewritten span.
//
// The gap is the whole guard against merging across a sentence boundary: a
// number is not spoken with a pause in the middle of it.
func joins(prev, next Token) bool {
	return next.Start-prev.End <= MaxGap
}

// trimPunct strips trailing punctuation for matching, and reports what it took
// so a rewrite can put it back. A punctuating model attaches marks to words,
// and "пять," must still match the unit "пять".
func trimPunct(s string) (word, tail string) {
	i := len(s)
	for i > 0 {
		r := rune(s[i-1])
		if r < 0x80 && strings.ContainsRune(".,!?;:", r) {
			i--
			continue
		}
		break
	}
	return s[:i], s[i:]
}
