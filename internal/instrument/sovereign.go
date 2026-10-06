package instrument

import "strings"

// russianExternalBonds are the bonds of the Russian Federation's external
// loans in a foreign currency, by ISIN: every «ГОВОЗ РФ» issue of Минфин
// России (MOEX ISS emitter 1228) the exchange knows, with a face in dollars or
// euros, read on 2026-10-07. The one rouble issue (XS0564087541) is not among
// them: the rule is for papers «номинированных в иностранной валюте».
var russianExternalBonds = map[string]bool{
	"RU000A0JWHA4": true, "RU000A0JXTS9": true, "RU000A0JXU14": true, "RU000A0ZYYN4": true,
	"RU000A0ZZVE6": true, "RU000A1006S9": true, "RU000A102CK5": true, "RU000A102CL3": true,
	"RU000A1034K8": true, "US78307AAE38": true, "US78307AAF03": true, "US78307AAG85": true,
	"US78307ACY73": true, "US78307ACZ49": true, "XS0077745163": true, "XS0088543193": true,
	"XS0089372063": true, "XS0089375249": true, "XS0114288789": true, "XS0114295560": true,
	"XS0504954180": true, "XS0504954347": true, "XS0767469827": true, "XS0767472458": true,
	"XS0767473852": true, "XS0971721377": true, "XS0971721450": true, "XS0971721963": true,
	"XS0971722342": true,
}

// RussianExternalBond reports a bond of the Russian Federation's external
// loans in a foreign currency, whose cost a Russian tax resident counts in
// roubles at the rate of the day of the sale (НК РФ ст. 214.1 п. 13).
func RussianExternalBond(isin string) bool {
	return russianExternalBonds[strings.ToUpper(strings.TrimSpace(isin))]
}
