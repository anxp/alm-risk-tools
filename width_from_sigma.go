package main

// width_from_sigma.go
//
// Реалізація формули W_recommended(sigma) з документа
// width_recommendation_from_sigma.md -- крок за кроком, з тими самими
// позначеннями (z, a(W), T_ref, p*), щоб код і документ читались як одне ціле.
//
// Запуск демонстрації: go run width_from_sigma.go

import (
	"math"
)

// ln1_0001 -- скільки логарифму ціни припадає на 1 тік (Крок 2 документа).
var ln1_0001 = math.Log(1.0001)

// ---------------------------------------------------------------------------
// КРОК 3-4 документа: нормальний розподіл -- Φ і Φ⁻¹
// ---------------------------------------------------------------------------
// Стандартна бібліотека Go має math.Erf (пряма функція помилок) і
// math.Erfinv (обернена) -- цього досить, щоб порахувати і Φ, і Φ⁻¹ точно,
// без наближених таблиць чи сторонніх залежностей.

// normalCDF -- Φ(z): кумулятивна функція стандартного нормального розподілу.
// Φ(z) = 0.5 * (1 + erf(z/√2))
func normalCDF(z float64) float64 {
	return 0.5 * (1.0 + math.Erf(z/math.Sqrt2))
}

// normalQuantile -- Φ⁻¹(p): обернена до Φ (потрібна для Кроку 6 -- переходу
// від цільового рівня ризику p* до z*).
// Виведення: p = 0.5*(1+erf(x/√2))  =>  x = √2 * erfinv(2p-1)
func normalQuantile(p float64) float64 {
	return math.Sqrt2 * math.Erfinv(2*p-1)
}

// ---------------------------------------------------------------------------
// КРОК 5: geometрія одностороннього діапазону -- a(W) = W * ln(1.0001)
// ---------------------------------------------------------------------------

// aOfWidth -- відстань до єдиної небезпечної межі в одиницях логарифму ціни.
func aOfWidth(widthTicks float64) float64 {
	return widthTicks * ln1_0001
}

// ---------------------------------------------------------------------------
// КРОК 4-5: z(W) і ймовірність пробою -- ті самі формули, що в документі,
// корисні для ПЕРЕВІРКИ (не для основного розрахунку W_recommended,
// а щоб можна було запитати "яка ймовірність пробою для ЦІЄЇ конкретної W?").
// ---------------------------------------------------------------------------

// zScore -- z = a(W) / (σ√T).
func zScore(widthTicks, sigmaDaily, tDays float64) float64 {
	if tDays <= 0 {
		return math.Inf(1) // за нульовий час пробити межу неможливо -> z=+inf, p=0
	}
	return aOfWidth(widthTicks) / (sigmaDaily * math.Sqrt(tDays))
}

// BreachProbability -- p = 2*(1-Φ(z)), ймовірність пробити ЄДИНУ межу за T днів.
func BreachProbability(widthTicks, sigmaDaily, tDays float64) float64 {
	z := zScore(widthTicks, sigmaDaily, tDays)
	return 2.0 * (1.0 - normalCDF(z))
}

// ---------------------------------------------------------------------------
// КРОК 6-7: головний результат -- W_recommended(sigma)
// ---------------------------------------------------------------------------

// RiskPolicy -- два свідомих параметри політики ризику (Крок 6 документа).
// Це НЕ результат розрахунку з даних, а вибір ризик-менеджменту, який задає
// сам оператор бота.
type RiskPolicy struct {
	TargetRiskP float64 // p* -- прийнятний рівень ризику пробою за горизонт TRefDays (0..1)
	TRefDays    float64 // T_ref -- горизонт часу в днях, на який хочемо мати гарантію
}

// Multiplier повертає константу C(p*, T_ref) = z*·√T_ref / ln(1.0001) з Кроку 7.
// Рахується один раз для заданої політики ризику (не залежить від sigma чи пулу).
func (rp RiskPolicy) Multiplier() float64 {
	zStar := normalQuantile(1 - rp.TargetRiskP/2) // Крок 6: z* = Φ⁻¹(1 - p*/2)
	return zStar * math.Sqrt(rp.TRefDays) / ln1_0001
}

// RecommendedWidth -- W_recommended(σ) = C(p*, T_ref) × σ_daily (Крок 7, формула в рамці).
func (rp RiskPolicy) RecommendedWidth(sigmaDaily float64) float64 {
	return rp.Multiplier() * sigmaDaily
}

// ---------------------------------------------------------------------------
// Інтеграція з наявною логікою: W_recommended як НИЖНЯ МЕЖА БЕЗПЕКИ
// (п.9.2 документа) -- комбінуємо через max() зі старим реактивним методом.
// ---------------------------------------------------------------------------

// CombineWithHistoricalWidth реалізує рекомендацію з п.9.2 документа:
// нова формула гарантує нижню межу, старий (реактивний, на основі
// історичного min/max тіка) метод і далі може розширювати діапазон більше.
func CombineWithHistoricalWidth(registeredRangeWidth int64, rp RiskPolicy, sigmaDaily float64) int64 {
	widthFromSigma := int64(math.Ceil(rp.RecommendedWidth(sigmaDaily)))
	if widthFromSigma > registeredRangeWidth {
		return widthFromSigma
	}
	return registeredRangeWidth
}
