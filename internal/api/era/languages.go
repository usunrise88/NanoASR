package era

// languageName renders a code the way upstream does: the lowercase English
// name whisper's tokenizer table carries.
//
// The table here is not that table. Whisper's has ninety-nine entries because
// whisper recognises ninety-nine languages; this server reports the language a
// model declares in its manifest, and the catalog declares a handful. Listing
// the ones that can occur plus the common neighbours, and falling back to the
// code itself, keeps the field readable without pretending to a coverage this
// build does not have.
func languageName(code string) string {
	if name, ok := languageNames[code]; ok {
		return name
	}
	return code
}

var languageNames = map[string]string{
	"multi": "multilingual",

	"ar": "arabic",
	"be": "belarusian",
	"bg": "bulgarian",
	"cs": "czech",
	"da": "danish",
	"de": "german",
	"el": "greek",
	"en": "english",
	"es": "spanish",
	"et": "estonian",
	"fa": "persian",
	"fi": "finnish",
	"fr": "french",
	"he": "hebrew",
	"hi": "hindi",
	"hu": "hungarian",
	"hy": "armenian",
	"id": "indonesian",
	"it": "italian",
	"ja": "japanese",
	"ka": "georgian",
	"kk": "kazakh",
	"ko": "korean",
	"lt": "lithuanian",
	"lv": "latvian",
	"nl": "dutch",
	"no": "norwegian",
	"pl": "polish",
	"pt": "portuguese",
	"ro": "romanian",
	"ru": "russian",
	"sk": "slovak",
	"sl": "slovenian",
	"sr": "serbian",
	"sv": "swedish",
	"tr": "turkish",
	"uk": "ukrainian",
	"uz": "uzbek",
	"vi": "vietnamese",
	"zh": "chinese",
}
