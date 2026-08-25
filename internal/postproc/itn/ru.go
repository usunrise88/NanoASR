package itn

import (
	"strconv"
	"strings"
)

func init() { Register(ruRules{}) }

// ruRules is the Russian rule set: numbers, money, percentages, times, dates
// and phone numbers (SPEC §5.6).
//
// Rules are tried longest-first, because "двадцать пять процентов" has to
// become "25%" rather than "25" followed by an untouched "процентов".
type ruRules struct{}

func (ruRules) Locale() string { return "ru" }

func (r ruRules) Match(in []Token) (Match, bool) {
	if len(in) == 0 {
		return Match{}, false
	}
	// Order is precedence. Each rule returns the number of tokens it consumed,
	// and the first that answers wins.
	for _, rule := range []func([]Token) (Match, bool){
		matchPhone,
		matchTime,
		matchDate,
		matchMoney,
		matchPercent,
		matchPlainNumber,
	} {
		if m, ok := rule(in); ok {
			return m, true
		}
	}
	return Match{}, false
}

// matchPlainNumber is the fallback: a run of number words becomes digits.
//
// Single small numbers are left alone. "у меня один вопрос" reads worse as "у
// меня 1 вопрос", and the same is true of "два" and "три" in ordinary prose;
// the value of ITN is in the numbers nobody wants to read as words.
func matchPlainNumber(in []Token) (Match, bool) {
	n, ok := parseNumber(in)
	if !ok {
		return Match{}, false
	}
	if n.count == 1 && n.value < 10 {
		return Match{}, false
	}
	_, tail := trimPunct(in[n.count-1].Text)
	return Match{Text: strconv.FormatInt(n.value, 10) + tail, Count: n.count}, true
}

// Currency words, mapped to the abbreviation that follows the amount.
var ruCurrency = map[string]string{
	"рубль": "руб.", "рубля": "руб.", "рублей": "руб.", "рублю": "руб.", "рублями": "руб.",
	"копейка": "коп.", "копейки": "коп.", "копеек": "коп.",
	"доллар": "$", "доллара": "$", "долларов": "$",
	"евро":  "€",
	"тенге": "₸",
}

// matchMoney rewrites "двадцать пять рублей" as "25 руб.".
func matchMoney(in []Token) (Match, bool) {
	n, ok := parseNumber(in)
	if !ok || n.count >= len(in) {
		return Match{}, false
	}
	next := in[n.count]
	if !joins(in[n.count-1], next) {
		return Match{}, false
	}
	word, tail := trimPunct(next.Lower)
	unit, ok := ruCurrency[word]
	if !ok {
		return Match{}, false
	}
	// The symbol currencies read better in front of the amount, the abbreviated
	// ones after it, which is how each is written in Russian.
	amount := strconv.FormatInt(n.value, 10)
	text := amount + " " + unit
	if unit == "$" || unit == "€" || unit == "₸" {
		text = unit + amount
	}
	return Match{Text: text + tail, Count: n.count + 1}, true
}

var ruPercent = map[string]bool{
	"процент": true, "процента": true, "процентов": true, "процентах": true,
}

func matchPercent(in []Token) (Match, bool) {
	n, ok := parseNumber(in)
	if !ok || n.count >= len(in) {
		return Match{}, false
	}
	next := in[n.count]
	if !joins(in[n.count-1], next) {
		return Match{}, false
	}
	word, tail := trimPunct(next.Lower)
	if !ruPercent[word] {
		return Match{}, false
	}
	return Match{Text: strconv.FormatInt(n.value, 10) + "%" + tail, Count: n.count + 1}, true
}

var ruMonths = map[string]int{
	"января": 1, "февраля": 2, "марта": 3, "апреля": 4, "мая": 5, "июня": 6,
	"июля": 7, "августа": 8, "сентября": 9, "октября": 10, "ноября": 11, "декабря": 12,
}

// Ordinals for days of the month, which are said as ordinals rather than as
// the cardinals parseNumber knows.
var ruDayOrdinals = map[string]int{
	"первое": 1, "первого": 1, "второе": 2, "второго": 2, "третье": 3, "третьего": 3,
	"четвёртое": 4, "четвертое": 4, "четвёртого": 4, "четвертого": 4,
	"пятое": 5, "пятого": 5, "шестое": 6, "шестого": 6, "седьмое": 7, "седьмого": 7,
	"восьмое": 8, "восьмого": 8, "девятое": 9, "девятого": 9, "десятое": 10, "десятого": 10,
	"одиннадцатое": 11, "одиннадцатого": 11, "двенадцатое": 12, "двенадцатого": 12,
	"тринадцатое": 13, "тринадцатого": 13, "четырнадцатое": 14, "четырнадцатого": 14,
	"пятнадцатое": 15, "пятнадцатого": 15, "шестнадцатое": 16, "шестнадцатого": 16,
	"семнадцатое": 17, "семнадцатого": 17, "восемнадцатое": 18, "восемнадцатого": 18,
	"девятнадцатое": 19, "девятнадцатого": 19, "двадцатое": 20, "двадцатого": 20,
	"тридцатое": 30, "тридцатого": 30,
	"двадцать": 20, "тридцать": 30, // compound: "двадцать пятого"
}

var ruCompoundDay = map[string]int{
	"первого": 1, "второго": 2, "третьего": 3, "четвёртого": 4, "четвертого": 4,
	"пятого": 5, "шестого": 6, "седьмого": 7, "восьмого": 8, "девятого": 9,
	"первое": 1, "второе": 2, "третье": 3, "четвёртое": 4, "четвертое": 4,
	"пятое": 5, "шестое": 6, "седьмое": 7, "восьмое": 8, "девятое": 9,
}

// matchDate rewrites "двадцать пятого декабря" as "25 декабря".
//
// The month name stays a word: "25.12" is a different register, and a date
// spoken in prose is written with the month spelled out.
func matchDate(in []Token) (Match, bool) {
	day, used, ok := parseDay(in)
	if !ok || used >= len(in) {
		return Match{}, false
	}
	next := in[used]
	if !joins(in[used-1], next) {
		return Match{}, false
	}
	word, tail := trimPunct(next.Lower)
	if _, ok := ruMonths[word]; !ok {
		return Match{}, false
	}
	// The month keeps the spelling it was recognised with, including its
	// capital if a punctuating model gave it one.
	month, _ := trimPunct(next.Text)
	return Match{
		Text:  strconv.Itoa(day) + " " + month + tail,
		Count: used + 1,
	}, true
}

// parseDay reads a day of the month, either simple ("двадцатого") or compound
// ("двадцать пятого").
func parseDay(in []Token) (day, used int, ok bool) {
	first, _ := trimPunct(in[0].Lower)

	if tens, isTens := map[string]int{"двадцать": 20, "тридцать": 30}[first]; isTens && len(in) > 1 && joins(in[0], in[1]) {
		second, _ := trimPunct(in[1].Lower)
		if unit, isUnit := ruCompoundDay[second]; isUnit {
			return tens + unit, 2, true
		}
	}
	if d, isDay := ruDayOrdinals[first]; isDay {
		// A bare "двадцать" is a cardinal, not a day; only the compound form
		// above makes it one.
		if first == "двадцать" || first == "тридцать" {
			return 0, 0, false
		}
		return d, 1, true
	}
	return 0, 0, false
}

var ruHourWords = map[string]bool{
	"час": true, "часа": true, "часов": true,
}

var ruMinuteWords = map[string]bool{
	"минута": true, "минуты": true, "минут": true,
}

// matchTime rewrites "девять часов тридцать минут" as "9:30".
func matchTime(in []Token) (Match, bool) {
	hours, ok := parseNumber(in)
	if !ok || hours.value > 23 || hours.count >= len(in) {
		return Match{}, false
	}
	next := in[hours.count]
	if !joins(in[hours.count-1], next) {
		return Match{}, false
	}
	word, _ := trimPunct(next.Lower)
	if !ruHourWords[word] {
		return Match{}, false
	}
	used := hours.count + 1

	// "девять часов" alone is a time, but writing it as "9:00" changes what
	// was said. Only an explicit minute count becomes a clock reading.
	if used >= len(in) || !joins(in[used-1], in[used]) {
		return Match{}, false
	}
	mins, ok := parseNumber(in[used:])
	if !ok || mins.value > 59 {
		return Match{}, false
	}
	used += mins.count
	if used >= len(in) || !joins(in[used-1], in[used]) {
		return Match{}, false
	}
	word, tail := trimPunct(in[used].Lower)
	if !ruMinuteWords[word] {
		return Match{}, false
	}
	return Match{
		Text:  strconv.FormatInt(hours.value, 10) + ":" + pad2(mins.value) + tail,
		Count: used + 1,
	}, true
}

func pad2(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) < 2 {
		return "0" + s
	}
	return s
}

// matchPhone rewrites a spoken digit run as a phone number.
//
// It fires only on a run that looks like one: a "плюс семь" opening, or ten or
// more consecutive digit words. Anything shorter is a number, and turning every
// digit sequence into a phone number would mangle account numbers and years.
func matchPhone(in []Token) (Match, bool) {
	i, digits := 0, strings.Builder{}
	plus := false

	if w, _ := trimPunct(in[0].Lower); w == "плюс" {
		plus = true
		i = 1
	}

	for i < len(in) {
		if i > 0 && !joins(in[i-1], in[i]) {
			break
		}
		w, _ := trimPunct(in[i].Lower)
		n, ok := ruUnits[w]
		if !ok || n > 9 {
			// Only single digits: a phone number is dictated digit by digit.
			break
		}
		digits.WriteString(strconv.FormatInt(n, 10))
		i++
	}

	count := digits.Len()
	if plus && count < 10 {
		return Match{}, false
	}
	if !plus && count < 10 {
		return Match{}, false
	}
	text := digits.String()
	if plus {
		text = "+" + text
	}
	_, tail := trimPunct(in[i-1].Text)
	return Match{Text: text + tail, Count: i}, true
}
