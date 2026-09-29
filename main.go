package main

import (
	"fmt"
	"math"
	"os"
)

// ---------------------------------------------------------------------------
// ГОЛОВНА ПРОГРАМА -- друкує кожен крок розрахунку послідовно
// ---------------------------------------------------------------------------

func main() {
	// --- шлях до CSV: або перший аргумент командного рядка, або значення тут ---
	csvPath := "tickHistory.csv"
	if len(os.Args) > 1 {
		csvPath = os.Args[1]
	}

	fmt.Printf("=== КРОК 0: завантаження і чистка даних ===\n")
	fmt.Printf("Файл: %s\n\n", csvPath)

	raw, err := loadTickHistory(csvPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Помилка: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Завантажено рядків (до чистки): %d\n", len(raw))

	normalized, stats := NormalizeTickHistory(raw) // ← нормалізація й пороги -- РІВНО ОДИН РАЗ
	rv, err := ComputeSigmaRealizedVariance(normalized, stats.GapThresholdSec)

	if err != nil {
		fmt.Fprintf(os.Stderr, "Помилка: %v\n", err)
		os.Exit(1)
	}

	spanDays := float64(normalized[len(normalized)-1].Timestamp-normalized[0].Timestamp) / 86400.0
	fmt.Printf("Період даних: %.1f днів (timestamp from %d to %d)\n\n", spanDays, normalized[0].Timestamp, normalized[len(normalized)-1].Timestamp)
	fmt.Printf("Після сортування і дедуплікації за timestamp: %d\n", len(normalized))
	fmt.Printf("Видалено дублікатів: %d\n", stats.RemovedDuplicates)
	fmt.Printf("Видалено коротких інтервалів (менше за %d сек): %d\n\n", stats.ShortIntervalThresholdSec, stats.RemovedShortIntervals)

	fmt.Printf("=== КРОК 2-3: реалізована варіація -> sigma_daily ===\n")
	fmt.Printf("Інтервалів використано: %d\n", rv.IntervalsUsed)
	fmt.Printf("Медіанне значення інтервалу: %d сек\n", stats.IntervalMedianValue)
	fmt.Printf("Розривів (> %d сек) виключено: %d\n", rv.GapThresholdSec, rv.GapsSkipped)
	fmt.Printf("Сумарний 'чистий' час: %.2f днів\n", rv.DaysUsedNet)
	fmt.Printf("sigma_daily (метод A) = sqrt(Σ[r_i²/(N * Δt_i)]) = %.4f\n\n", rv.SigmaDaily)

	fmt.Printf("=== КРОК 4: крос-перевірка (годинний ресемплінг + stdev) ===\n")

	sigmaB, nReturns := computeSigmaHourlyResample(normalized)
	fmt.Printf("Годинних приростів використано: %d\n", nReturns)
	fmt.Printf("sigma_daily (метод B) = sigma_hourly * sqrt(24) = %.4f\n", sigmaB)

	diffPct := math.Abs(rv.SigmaDaily-sigmaB) / rv.SigmaDaily * 100
	fmt.Printf("Розбіжність між методами: %.1f%% %s\n\n", diffPct,
		func() string {
			if diffPct < 10 {
				return "(добре -- методи узгоджені)"
			}
			return "(велика розбіжність -- варто перевірити дані)"
		}())

	fmt.Printf("=== КРОК 5: стабільність sigma в часі (rolling-вікна) ===\n")
	fmt.Printf("%12s %12s\n", "Вікно", "sigma_daily")
	windows := []int{int(spanDays), 90, 60, 30, 14, 7}
	seen := make(map[int]bool)
	for _, w := range windows {
		if w <= 0 || seen[w] {
			continue
		}

		seen[w] = true

		sub := normalized.FilterWindow(w) // ← лише фільтр за часом, без повторної обробки
		if len(sub) < 2 {
			continue
		}
		rvW, err := ComputeSigmaRealizedVariance(sub, stats.GapThresholdSec) // ← той самий поріг всюди
		if err != nil {
			fmt.Fprintf(os.Stderr, "Помилка: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("%9d днів %12.4f  (даних використано: %.1f днів)\n", w, rvW.SigmaDaily, rvW.DaysUsedNet)
	}

	fmt.Printf("\n=== ПІДСУМОК ===\n")
	fmt.Printf("Рекомендоване sigma_daily для range_width_calculator.go: %.4f\n", rv.SigmaDaily)
	fmt.Printf("(або візьми значення з коротшого вікна вище, якщо вважаєш поточний режим волатильності відмінним від середнього за весь період)\n\n")

	// ---------------------------------------------------------------------------
	// ДЕМОНСТРАЦІЯ -- відтворює таблицю з розділу 8 документа + перевірку z*
	// ---------------------------------------------------------------------------
	fmt.Printf("Частина 2: Обчислення рекомендованої ширини діапазону для заданої sigma\n")

	// --- Перевірка normalQuantile на еталонних значеннях (з розділу 8 документа) ---
	fmt.Println("=== Перевірка Φ⁻¹ на відомих довірчих рівнях ===")
	for _, p := range []float64{0.05, 0.10, 0.32} {
		z := normalQuantile(1 - p/2)
		fmt.Printf("p*=%5.2f%%  ->  z* = %.4f\n", p*100, z)
	}
	fmt.Println("(звіряй: 5% -> 1.9600, 10% -> 1.6449 -- стандартні табличні значення)\n")

	// --- Таблиця множників (розділ 8 документа) ---
	fmt.Println("=== Таблиця W_recommended для sigma=0.05 і sigma=0.02 ===")
	fmt.Printf("%8s %6s %10s %14s %14s\n", "T_ref", "p*", "C (множник)", "W_rec(σ=0.05)", "W_rec(σ=0.02)")
	for _, tRef := range []float64{5, 10, 20} {
		for _, p := range []float64{0.10, 0.15, 0.20} {
			rp := RiskPolicy{TargetRiskP: p, TRefDays: tRef}
			c := rp.Multiplier()
			fmt.Printf("%8.0f %5.0f%% %10.1f %14.0f %14.0f\n",
				tRef, p*100, c, rp.RecommendedWidth(0.05), rp.RecommendedWidth(0.02))
		}
	}

	// --- Приклад: інтеграція з наявним registeredRangeWidth (ETH/ZRO кейс) ---
	fmt.Println("\n=== Приклад: ETH/ZRO, T_ref=10, p*=15% ===")
	rp := RiskPolicy{TargetRiskP: 0.15, TRefDays: 10}
	sigmaRecent := 0.05                 // підстав тут реальну ковзну (14-30 днів) sigma_daily пулу
	registeredRangeWidth := int64(2500) // те, що дав би старий реактивний метод

	final := CombineWithHistoricalWidth(registeredRangeWidth, rp, sigmaRecent)
	fmt.Printf("Реактивний метод дав би:      %d ticks\n", registeredRangeWidth)
	fmt.Printf("Формула від sigma рекомендує: %.0f ticks\n", rp.RecommendedWidth(sigmaRecent))
	fmt.Printf("Фінальна (max з двох):        %d ticks\n", final)

	// --- Перевірка: яка фактична ймовірність пробою для фінальної ширини? ---
	pFinal := BreachProbability(float64(final), sigmaRecent, rp.TRefDays)
	fmt.Printf("Фактична P(пробій за %.0f днів) для цієї ширини: %.2f%% (мало б бути ~%.0f%%)\n",
		rp.TRefDays, pFinal*100, rp.TargetRiskP*100)
}
