// sigma_calculator.go
//
// Розрахунок sigma_daily (денної волатильності логарифму ціни) з реального
// tickHistory CSV-файлу (формат: "Timestamp|int64,Tick|int64" + рядки timestamp,tick).
//
// Послідовність кроків детально пояснена в sigma_daily_from_tickhistory.md,
// що йде поруч з цим файлом. Тут -- той самий розрахунок кодом, крок за кроком,
// з друком проміжних результатів на кожному етапі (щоб бачити ХІД розрахунку,
// а не лише фінальне число).
//
// Запуск: go run sigma_calculator.go /шлях/до/твого/tickHistory.csv

package main

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	array_basics "github.com/anxp/array-basics"
)

// TickPoint - один запис з tickHistory: момент часу (unix seconds) і тік пулу.
type TickPoint struct {
	Timestamp int64
	Tick      int64
}

// ---------------------------------------------------------------------------
// КРОК 0a: завантаження CSV
// ---------------------------------------------------------------------------

func loadTickHistory(path string) ([]TickPoint, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("не вдалось відкрити файл: %w", err)
	}
	defer f.Close()

	var points []TickPoint
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // на випадок довгих рядків

	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		parts := strings.Split(line, ",")
		if len(parts) != 2 {
			continue // пропускаємо криві рядки
		}

		ts, err1 := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
		tick, err2 := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err1 != nil || err2 != nil {
			// найімовірніше це заголовок "Timestamp|int64,Tick|int64" -- пропускаємо мовчки
			continue
		}

		points = append(points, TickPoint{Timestamp: ts, Tick: tick})
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("помилка читання файлу: %w", err)
	}

	return points, nil
}

// normalizeTickHistory - prepare input data - sort, remove duplicates, remove too short intervals.
//
//	points - input (raw) data set
//
//	filteredData - sorted data set, filtered from duplicates and short intervals.
//	removedDuplicates - number of removed duplicates.
//	removedShortIntervals - number of removed short intervals.
func normalizeTickHistory(rawPoints []TickPoint) (filteredData []TickPoint, intervalMedianValue int64, intervalFilteringLowerThreshold int64, removedDuplicates int64, removedShortIntervals int64) {
	const ShortIntervalThresholdFactor = 10

	sort.Slice(rawPoints, func(i, j int) bool {
		return rawPoints[i].Timestamp < rawPoints[j].Timestamp
	})

	deltaT := make([]int64, len(rawPoints)-1, len(rawPoints)-1)
	for i := 1; i < len(rawPoints)-1; i++ {
		deltaT[i] = rawPoints[i].Timestamp - rawPoints[i-1].Timestamp
	}

	intervalMedianValue = array_basics.FindMedian[int64](deltaT)
	skipIntervalsLessThanXSec := intervalMedianValue / ShortIntervalThresholdFactor // Skip too short intervals.

	filteredData = make([]TickPoint, 0, len(rawPoints))

	for _, rawPoint := range rawPoints {
		l := len(filteredData)
		duplicateFound := l > 0 && (filteredData[l-1].Timestamp == rawPoint.Timestamp)
		shortIntervalFound := l > 0 && skipIntervalsLessThanXSec > 0 && rawPoint.Timestamp-filteredData[l-1].Timestamp < skipIntervalsLessThanXSec

		if duplicateFound {
			removedDuplicates++
			filteredData[len(filteredData)-1] = rawPoint
		} else if shortIntervalFound {
			removedShortIntervals++
			// Here, we do not handle situations where significant changes are recorded over
			// a SHORT time interval - we simply take the latest reading. This is acceptable,
			// as the price measurement accuracy on a STANDARD (NORMAL) time intervals is not better.
			filteredData[len(filteredData)-1] = rawPoint
		} else {
			filteredData = append(filteredData, rawPoint)
		}
	}

	return filteredData, intervalMedianValue, skipIntervalsLessThanXSec, removedDuplicates, removedShortIntervals
}

// ---------------------------------------------------------------------------
// КРОК 1: тік -> логарифм ціни
// ---------------------------------------------------------------------------

func convertTickToLnOfPrice(tick int64) float64 {
	// Here we just change BASE of logarithm from 1.0001 to e:
	// log_e(price) = log_{1.0001}(price) × log_e(1.0001) = tick × ln(1.0001)
	var ln1_0001 = math.Log(1.0001)
	return float64(tick) * ln1_0001
}

// ---------------------------------------------------------------------------
// КРОК 2-3: реалізована варіація (realized variance) -> sigma_daily
// ---------------------------------------------------------------------------
// Розбиваємо ряд на "чисті" сегменти по розривах довше GapThresholdSec.
// Усередині кожного сегмента рахуємо r_i (лог-приріст ціни між двома сусідніми
// точками) і dt_i (інтервал між двома сусідніми точками в днях),
// підсумовуємо r_i^2/dt_i по формулі σ = sqrt(Σ[r_i²/(N * Δt_i)]).

// RealizedVariance - проміжні числа розрахунку, щоб бачити хід обчислення.
type RealizedVariance struct {
	SigmaDaily            float64
	DaysUsedNet           float64
	IntervalMedianValue   int64
	IntervalsUsed         int64
	GapsSkipped           int64
	GapThresholdSec       int64 // Every time frame, larger than this threshold considered as GAP and not taken into calculation.
	LowerThresholdSec     int64 // Every interval shorter than LowerThresholdSec considered as "noise" (maybe data from repeated request?) and skipped.
	RemovedDuplicates     int64
	RemovedShortIntervals int64
	FilteredPoints        *[]TickPoint
}

func computeSigmaRealizedVariance(points []TickPoint) (RealizedVariance, error) {
	const GapThresholdMultiplier = 10

	filteredData, intervalMedianValue, intervalFilteringLowerThreshold, removedDuplicates, removedShortIntervals := normalizeTickHistory(points)

	if len(filteredData) < 2 {
		return RealizedVariance{}, fmt.Errorf("too few filtered data for calculations")
	}

	gapThresholdSec := GapThresholdMultiplier * intervalMedianValue
	gapsSkipped := int64(0)
	rXrNormalizedByDaySum := float64(0)
	daysUsedNet := float64(0)
	intervalsUsed := int64(0)
	sigmaDaily := float64(0)

	for i := 1; i < len(filteredData); i++ {
		dtSec := filteredData[i].Timestamp - filteredData[i-1].Timestamp
		if dtSec <= 0 {
			continue
		}
		if dtSec > gapThresholdSec {
			gapsSkipped++
			continue // великий розрив -- виключаємо цей інтервал з розрахунку
		}

		// Δt expressed (recalculated) in days
		dtDays := float64(dtSec) / 86400.0
		r := convertTickToLnOfPrice(filteredData[i].Tick) - convertTickToLnOfPrice(filteredData[i-1].Tick)
		rXrNormalizedByDaySum += (r * r) / dtDays

		daysUsedNet += dtDays
		intervalsUsed++
	}

	if daysUsedNet > 0 {
		// σ = sqrt(Σ[r_i²/(N * Δt_i)])
		sigmaDaily = math.Sqrt(rXrNormalizedByDaySum / float64(intervalsUsed))
	}

	return RealizedVariance{
		SigmaDaily:            sigmaDaily,
		DaysUsedNet:           daysUsedNet,
		IntervalMedianValue:   intervalMedianValue,
		IntervalsUsed:         intervalsUsed,
		GapsSkipped:           gapsSkipped,
		GapThresholdSec:       gapThresholdSec,
		LowerThresholdSec:     intervalFilteringLowerThreshold,
		RemovedDuplicates:     removedDuplicates,
		RemovedShortIntervals: removedShortIntervals,
		FilteredPoints:        &filteredData,
	}, nil
}

// ---------------------------------------------------------------------------
// КРОК 4: крос-перевірка -- ресемплінг на годинну сітку + класичний stdev
// ---------------------------------------------------------------------------

// resampleHourlyLastValue групує точки по годинних "бакетах" (timestamp/3600),
// в кожному бакеті лишає ОСТАННІЙ записаний тік, і розбиває на сегменти там,
// де пропущено більше однієї сусідньої години (тобто теж виключає розриви).
func resampleHourlyLastValue(points []TickPoint) [][]float64 {
	buckets := make(map[int64]int64)
	for _, p := range points {
		bucket := p.Timestamp / 3600
		buckets[bucket] = p.Tick // перезапис -> залишається останній запис у цій годині
	}

	keys := make([]int64, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	var segments [][]float64
	var current []float64
	var prevKey int64
	first := true

	for _, k := range keys {
		if !first && (k-prevKey) > 1 {
			if len(current) > 1 {
				segments = append(segments, current)
			}
			current = nil
		}
		current = append(current, convertTickToLnOfPrice(buckets[k]))
		prevKey = k
		first = false
	}
	if len(current) > 1 {
		segments = append(segments, current)
	}

	return segments
}

func computeSigmaHourlyResample(points []TickPoint) (sigmaDaily float64, nReturns int) {
	segments := resampleHourlyLastValue(points)

	var returns []float64
	for _, seg := range segments {
		for i := 1; i < len(seg); i++ {
			returns = append(returns, seg[i]-seg[i-1])
		}
	}

	if len(returns) == 0 {
		return 0, 0
	}

	// класичне (population) стандартне відхилення
	mean := 0.0
	for _, r := range returns {
		mean += r
	}
	mean /= float64(len(returns))

	sumSq := 0.0
	for _, r := range returns {
		d := r - mean
		sumSq += d * d
	}
	sigmaHourly := math.Sqrt(sumSq / float64(len(returns)))

	sigmaDaily = sigmaHourly * math.Sqrt(24.0) // правило кореня з часу: 24 години в добі
	return sigmaDaily, len(returns)
}

// ---------------------------------------------------------------------------
// КРОК 5: перевірка стабільності sigma в часі (rolling-вікна)
// ---------------------------------------------------------------------------

func computeSigmaForWindow(points []TickPoint, windowDays int) (RealizedVariance, bool) {
	if len(points) == 0 {
		return RealizedVariance{}, false
	}

	lastTs := points[len(points)-1].Timestamp
	cutoff := lastTs - int64(windowDays)*86400

	var sub []TickPoint
	for _, p := range points {
		if p.Timestamp >= cutoff {
			sub = append(sub, p)
		}
	}
	if len(sub) < 2 {
		return RealizedVariance{}, false
	}

	rv, err := computeSigmaRealizedVariance(sub)
	if err != nil {
		return RealizedVariance{}, false
	}

	return rv, true
}

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
	spanDays := float64(raw[len(raw)-1].Timestamp-raw[0].Timestamp) / 86400.0
	fmt.Printf("Період даних: %.1f днів (timestamp from %d to %d)\n\n", spanDays, raw[0].Timestamp, raw[len(raw)-1].Timestamp)

	rv, err := computeSigmaRealizedVariance(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Помилка: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Після сортування і дедуплікації за timestamp: %d\n", len(*rv.FilteredPoints))
	fmt.Printf("Видалено дублікатів: %d\n", rv.RemovedDuplicates)
	fmt.Printf("Видалено коротких інтервалів (менше за %d сек): %d\n\n", rv.LowerThresholdSec, rv.RemovedShortIntervals)

	fmt.Printf("=== КРОК 2-3: реалізована варіація -> sigma_daily ===\n")
	fmt.Printf("Інтервалів використано: %d\n", rv.IntervalsUsed)
	fmt.Printf("Медіанне значення інтервалу: %d сек\n", rv.IntervalMedianValue)
	fmt.Printf("Розривів (> %d сек) виключено: %d\n", rv.GapThresholdSec, rv.GapsSkipped)
	fmt.Printf("Сумарний 'чистий' час: %.2f днів\n", rv.DaysUsedNet)
	fmt.Printf("sigma_daily (метод A) = sqrt(Σ[r_i²/(N * Δt_i)]) = %.4f\n\n", rv.SigmaDaily)

	fmt.Printf("=== КРОК 4: крос-перевірка (годинний ресемплінг + stdev) ===\n")

	points := *rv.FilteredPoints // A COPY of value
	sigmaB, nReturns := computeSigmaHourlyResample(points)
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
		rvW, ok := computeSigmaForWindow(points, w)
		if !ok {
			continue
		}
		fmt.Printf("%9d днів %12.4f  (даних використано: %.1f днів)\n", w, rvW.SigmaDaily, rvW.DaysUsedNet)
	}

	fmt.Printf("\n=== ПІДСУМОК ===\n")
	fmt.Printf("Рекомендоване sigma_daily для range_width_calculator.go: %.4f\n", rv.SigmaDaily)
	fmt.Printf("(або візьми значення з коротшого вікна вище, якщо вважаєш поточний режим волатильності відмінним від середнього за весь період)\n")
}
