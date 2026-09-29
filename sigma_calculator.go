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

// NormalizedTickHistory -- дані, які вже пройшли через NormalizeTickHistory:
// відсортовані, дедубльовані, очищені від підозріло коротких інтервалів.
// ComputeSigmaRealizedVariance приймає САМЕ цей тип -- тому сирий, необроблений
// []TickPoint туди просто НЕ ВІЙДЕ без явного перетворення типу, яке одразу
// впаде в очі на code review.
type NormalizedTickHistory []TickPoint

// NormalizationStats -- усе, що встановлюється РІВНО ОДИН РАЗ на етапі нормалізації:
// медіана інтервалу і похідні від неї пороги. Рахується один раз на весь датасет
// і потім передається явно в кожен виклик ComputeSigmaRealizedVariance, щоб усі
// під-вікна використовували ОДНАКОВЕ означення "короткого інтервалу" й "розриву".
type NormalizationStats struct {
	IntervalMedianValue       int64
	GapThresholdSec           int64
	ShortIntervalThresholdSec int64
	RemovedDuplicates         int64
	RemovedShortIntervals     int64
}

// NormalizeTickHistory - prepare input data - sort, remove duplicates, remove too short intervals.
//
//	rawPoints - input (raw) data set
//	NormalizedTickHistory - sorted data set, filtered from duplicates and short intervals.
//	NormalizationStats - statistic data that should be calculated only once - median value of interval etc.
func NormalizeTickHistory(rawPoints []TickPoint) (NormalizedTickHistory, NormalizationStats) {
	const ThresholdFactor = 10 // симетричний фактор: medianDelta/10 -- "занадто коротко", medianDelta*10 -- "розрив"

	sort.Slice(rawPoints, func(i, j int) bool {
		return rawPoints[i].Timestamp < rawPoints[j].Timestamp
	})

	deltaT := make([]int64, len(rawPoints)-1)
	for i := 1; i < len(rawPoints); i++ {
		deltaT[i-1] = rawPoints[i].Timestamp - rawPoints[i-1].Timestamp
	}

	medianDelta := array_basics.FindMedian[int64](deltaT)
	shortThreshold := medianDelta / ThresholdFactor
	gapThreshold := medianDelta * ThresholdFactor

	filteredData := make([]TickPoint, 0, len(rawPoints))
	var removedDuplicates, removedShortIntervals int64

	for _, rawPoint := range rawPoints {
		l := len(filteredData)
		duplicateFound := l > 0 && filteredData[l-1].Timestamp == rawPoint.Timestamp
		shortIntervalFound := l > 0 && shortThreshold > 0 && rawPoint.Timestamp-filteredData[l-1].Timestamp < shortThreshold

		switch {
		case duplicateFound:
			removedDuplicates++
			filteredData[l-1] = rawPoint
		case shortIntervalFound:
			removedShortIntervals++
			// Here, we do not handle situations where significant changes are recorded over
			// a SHORT time interval - we simply take the latest reading. This is acceptable,
			// as the price measurement accuracy on a STANDARD (NORMAL) time intervals is not better.
			filteredData[l-1] = rawPoint
		default:
			filteredData = append(filteredData, rawPoint)
		}
	}

	return filteredData, NormalizationStats{
		IntervalMedianValue:       medianDelta,
		GapThresholdSec:           gapThreshold,
		ShortIntervalThresholdSec: shortThreshold,
		RemovedDuplicates:         removedDuplicates,
		RemovedShortIntervals:     removedShortIntervals,
	}
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
	SigmaDaily      float64
	DaysUsedNet     float64
	IntervalsUsed   int64
	GapsSkipped     int64
	GapThresholdSec int64 // Every time frame, larger than this threshold considered as GAP and not taken into calculation.
}

// ComputeSigmaRealizedVariance вимагає ВЖЕ нормалізовані дані (див. NormalizeTickHistory)
// і явний поріг розриву -- саме тому один і той самий поріг можна свідомо
// перевикористати для кількох під-вікон одного датасету, забезпечуючи чесне порівняння.
func ComputeSigmaRealizedVariance(points NormalizedTickHistory, gapThresholdSec int64) (RealizedVariance, error) {
	if len(points) < 2 {
		return RealizedVariance{}, fmt.Errorf("too few normalized data points for calculation")
	}

	var gapsSkipped, intervalsUsed int64
	var rXrNormalizedByDaySum, daysUsedNet float64

	for i := 1; i < len(points); i++ {
		dtSec := points[i].Timestamp - points[i-1].Timestamp
		if dtSec <= 0 {
			continue
		}
		if dtSec > gapThresholdSec {
			gapsSkipped++
			continue
		}

		dtDays := float64(dtSec) / 86400.0
		r := convertTickToLnOfPrice(points[i].Tick) - convertTickToLnOfPrice(points[i-1].Tick)
		rXrNormalizedByDaySum += (r * r) / dtDays

		daysUsedNet += dtDays
		intervalsUsed++
	}

	var sigmaDaily float64
	if daysUsedNet > 0 {
		sigmaDaily = math.Sqrt(rXrNormalizedByDaySum / float64(intervalsUsed))
	}

	return RealizedVariance{
		SigmaDaily:      sigmaDaily,
		DaysUsedNet:     daysUsedNet,
		IntervalsUsed:   intervalsUsed,
		GapsSkipped:     gapsSkipped,
		GapThresholdSec: gapThresholdSec,
	}, nil
}

// ---------------------------------------------------------------------------
// КРОК 4: крос-перевірка -- ресемплінг на годинну сітку + класичний stdev
// ---------------------------------------------------------------------------

// resampleHourlyLastValue групує точки по годинних "бакетах" (timestamp/3600),
// в кожному бакеті лишає ОСТАННІЙ записаний тік, і розбиває на сегменти там,
// де пропущено більше однієї сусідньої години (тобто теж виключає розриви).
func resampleHourlyLastValue(points NormalizedTickHistory) [][]float64 {
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

func computeSigmaHourlyResample(points NormalizedTickHistory) (sigmaDaily float64, nReturns int) {
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

// FilterWindow повертає підмножину вже нормалізованих даних за останні windowDays днів.
// Дані вже відсортовані й очищені -- тому результат теж НЕ потребує повторної
// нормалізації (жодного повторного сортування/дедублікації/пошуку медіани).
func (points NormalizedTickHistory) FilterWindow(windowDays int) NormalizedTickHistory {
	if len(points) == 0 {
		return nil
	}
	cutoff := points[len(points)-1].Timestamp - int64(windowDays)*86400
	idx := sort.Search(len(points), func(i int) bool { return points[i].Timestamp >= cutoff })
	return points[idx:]
}
