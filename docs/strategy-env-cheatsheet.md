# Watchtower — управление стратегиями через `.env`

Один файл. Что менять, какой эффект, как откатить. Никаких секретов в примерах.

## 1. Главный рубильник (4 безопасных значения по умолчанию)

```bash
STRATEGY_LEARNING_LOOP_PROMOTION_ALLOWED=false   # не пускать стратегии в live
STRATEGY_PROMOTION_BYPASS_EXPLICIT=false          # kill-switch гейта (оставить false)
TELEGRAM_STRATEGY_USER_FLOW_ENABLED=false         # user-канал молчит
STRATEGY_SHADOW_RECORD_NOFIRE=false               # не писать no-fire строки
```

**Все четыре `true` одновременно = production-небезопасный режим.** Pin-тест `TestEnvFiles_DangerousDefaultsBlocked` падает, если кто-то их поднял.

## 2. Включить / выключить стратегию

Каждая из 9 стратегий имеет два ключа: `*_ENABLED` (вкл/выкл) и `*_SHADOW_ONLY` (только shadow vs может стать live):

| Стратегия | ENABLED ключ | SHADOW_ONLY ключ |
|---|---|---|
| thesisaccum | `THESIS_ACCUM_ENABLED` | `THESIS_ACCUM_SHADOW_ONLY` |
| holderdelta | `OWNERSHIP_V2_ENABLED` | `OWNERSHIP_V2_SHADOW_ONLY` |
| catalystwindow | `CATALYST_WINDOW_ENABLED` | `CATALYST_WINDOW_SHADOW_ONLY` |
| bookvacuum | `BOOK_VACUUM_ENABLED` | `BOOK_VACUUM_SHADOW_ONLY` |
| repricinglag | `REPRICING_LAG_ENABLED` | `REPRICING_LAG_SHADOW_ONLY` |
| walletcohort | `WALLET_COHORT_ENABLED` | `WALLET_COHORT_SHADOW_ONLY` |
| conflictresolve | `CONFLICT_RESOLVE_ENABLED` | `CONFLICT_RESOLVE_SHADOW_ONLY` |
| rulesrisk | `RULES_RISK_ENABLED` | (всегда tag-only) |
| cheaptail | `CHEAPTAIL_ENABLED` | `CHEAPTAIL_SHADOW_ONLY` |

**Production-safe default**: `*_ENABLED=true`, `*_SHADOW_ONLY=true`.

**Полное отключение стратегии**: `*_ENABLED=false`. Никаких shadow rows, никаких vacuum-проверок, ничего.

## 3. Перевести стратегию в live (после прохождения promotion)

ВСЕ ТРИ условия должны быть выполнены одновременно:

```bash
STRATEGY_LEARNING_LOOP_PROMOTION_ALLOWED=true   # глобальный гейт
THESIS_ACCUM_SHADOW_ONLY=false                  # пер-стратегийный гейт
# + автоматически: promotion review должен дать eligible=true для thesisaccum
```

Без 3-го условия (eligible) bus всё равно принудительно ставит `shadow_only=true` на каждую запись. Это triple-lock.

Проверить eligible:
```sql
SELECT strategy_name, sample_size, median_signed_move_6h, eligible
FROM polymarket_strategy_promotion_reviews
WHERE reviewed_at > NOW() - INTERVAL '2 hours'
ORDER BY reviewed_at DESC;
```

## 4. Тюнинг порогов (что менять и какой эффект)

### thesisaccum (cross-market accumulation)
```bash
THESIS_ACCUM_MIN_BREADTH=2          # минимум линкованных рынков. ↑ → меньше fires
THESIS_ACCUM_MIN_CONSISTENCY=0.75   # aligned/(aligned+opposed). ↑ → чище сигнал
THESIS_ACCUM_MIN_ALIGNED_SCORE=1.5  # USD floor aligned exposure. ↑ → меньше шума
```

### holderdelta (top-holder concentration)
```bash
OWNERSHIP_V2_MIN_PCT_OI_INFO=0.03   # 3% OI. ↑ → только крупные holders
OWNERSHIP_V2_MIN_PCT_OI_WARN=0.08   # 8% для warning
OWNERSHIP_V2_MIN_PCT_OI_CRIT=0.15   # 15% для critical
OWNERSHIP_V2_TOPK=5                 # сколько top-холдеров отслеживать
```

### catalystwindow (booster — никогда standalone)
```bash
CATALYST_WINDOW_MIN_CONFIDENCE=0.5  # AI-confidence катализатора
CATALYST_WINDOW_DEBATE_PRE=12h      # окно до дебатов
CATALYST_WINDOW_DEBATE_POST=4h      # окно после
CATALYST_WINDOW_COURT_RULING_PRE=24h
CATALYST_WINDOW_ELECTION_DAY_PRE=72h
```

### bookvacuum (depth collapse)
```bash
BOOK_VACUUM_MIN_COLLAPSE_PCT=0.5    # ≥50% top-N исчезает. ↑ → строже
BOOK_VACUUM_MIN_SPREAD_Z=1.5        # σ от baseline spread
BOOK_VACUUM_MAX_RESTORE_SEC=30      # если глубина вернулась за <30с → не fire
```

### repricinglag (post-news underreaction)
```bash
REPRICING_LAG_MIN_CENTS=3           # минимум lag в центах
REPRICING_LAG_PEER_MIN_COUNT=2      # минимум peer-рынков
REPRICING_LAG_MAX_AMBIGUITY=0.6     # выше → блокируется rulesrisk
```

### walletcohort (behavioural co-trade)
```bash
WALLET_COHORT_MIN_SIMILARITY=0.5    # порог edge similarity
WALLET_COHORT_MIN_EVENTS=3          # минимум общих событий
```

### conflictresolve (arbitration)
```bash
CONFLICT_RESOLVE_MIN_DOMINANCE=1.5  # ratio для keep-winner
CONFLICT_RESOLVE_MM_PENALTY=0.4     # штраф MM-стороне
```

### rulesrisk (safety — блокирует другие)
```bash
RULES_RISK_HIGH_THRESHOLD=0.6       # выше → high ambiguity
RULES_RISK_BLOCK_REPRICING=true     # high → блок repricinglag
RULES_RISK_BLOCK_CHEAPTAIL=true     # high → блок cheaptail
```

### cheaptail (low-prob staging)
```bash
CHEAPTAIL_MIN_PROB=0.02             # band 2¢-15¢
CHEAPTAIL_MAX_PROB=0.15
CHEAPTAIL_MIN_NOTIONAL_USD=1000     # non-dust floor
CHEAPTAIL_MIN_TRADES=2              # минимум staging-сделок
CHEAPTAIL_REQUIRE_CATALYST=true     # обязательный catalyst поблизости
```

## 5. Telegram: admin vs user

```bash
# Admin (debug/operator surface)
TELEGRAM_STRATEGY_ADMIN_FLOW_ENABLED=true       # включить admin
TELEGRAM_STRATEGY_SHADOW_TO_ADMIN=true          # shadow rows → admin
TELEGRAM_STRATEGY_ADMIN_DEDUPE_WINDOW=1h        # дедуп admin

# User (только promoted, default OFF)
TELEGRAM_STRATEGY_USER_FLOW_ENABLED=false       # MUST=false пока не promoted
TELEGRAM_STRATEGY_PROMOTED_TO_USER=true         # promoted → user (когда unlock)
TELEGRAM_STRATEGY_MIN_USER_CONFIDENCE=0.75      # floor confidence
TELEGRAM_STRATEGY_MIN_USER_LEVEL=warning        # floor severity
TELEGRAM_STRATEGY_USER_DEDUPE_WINDOW=12h        # дедуп user (агрессивный)
```

## 6. Воркеры (data producers)

```bash
# Holdersync — поллит Polymarket /holders
HOLDERSYNC_WORKER_ENABLED=true
HOLDERSYNC_SOURCE_MODE=dataapi      # или "disabled" — kill switch
HOLDERSYNC_INTERVAL=10m
HOLDERSYNC_MAX_MARKETS=100          # ↑ → больше покрытие, больше cost
HOLDERSYNC_RATE_LIMIT_RPS=2

# Bookbars — поллит CLOB /book (cost hotspot!)
BOOK_FEATURE_BARS_ENABLED=true
BOOK_FEATURE_BARS_INTERVAL=15s      # 5s default слишком агрессивно
BOOK_FEATURE_BARS_MAX_MARKETS=100   # стартовать с 100, не 250
BOOK_FEATURE_BARS_TOPN=5

# Остальные
MARKETLINKS_ENABLED=true            # граф связанных рынков
THESIS_LINES_WORKER_ENABLED=true    # агрегаты per-wallet × event
WALLETGRAPH_ENABLED=true            # co-trade edges
RISKSCORE_ENABLED=true              # ambiguity scoring
REPRICING_WORKER_ENABLED=true       # open/close windows
REPRICING_CLOSE_ENABLED=true        # real price sampler
```

## 7. Kill-switches (emergency rollback)

Если что-то идёт не так — выключить без перезапуска кода:

```bash
# Полный stop strategy-слоя:
STRATEGY_STAGED_INPUTS_ENABLED=false   # все non-rulesrisk стратегии перестанут оцениваться
HOLDERSYNC_WORKER_ENABLED=false
BOOK_FEATURE_BARS_ENABLED=false
THESIS_LINES_WORKER_ENABLED=false

# Тихий Telegram:
TELEGRAM_STRATEGY_ADMIN_FLOW_ENABLED=false
TELEGRAM_STRATEGY_USER_FLOW_ENABLED=false

# Старый v4 alerting pipeline ПРОДОЛЖИТ работать — strategy rollback его не трогает.
```

## 8. Проверка после изменения `.env`

```bash
# 1. Тест dangerous defaults (должен пройти)
go test ./internal/app/ -run TestEnvFiles -count 1

# 2. Sync .env и .env.example
bash scripts/audit-env.sh

# 3. Полная проверка
bash scripts/verify-local.sh
```

Если `TestEnvFiles_DangerousDefaultsBlocked` падает — значит один из 4 главных рубильников из §1 поднят. Это safety net, не баг.

## 9. Что мониторить после изменений

| Изменили | Смотреть |
|---|---|
| Threshold стратегии | `polymarket_strategy_shadow_decisions` rows per hour для этой стратегии |
| Worker интервал/лимит | freshness таблицы воркера (holder_snapshots, book_feature_bars, …) |
| Promotion gate | `polymarket_strategy_promotion_reviews` latest row, `eligible` колонка |
| User-flow конфиг | счётчик user-сообщений за 24h (должен быть единицы, не десятки) |
| Telegram-флаг | admin/user msg count в Grafana |

SQL для всех этих проверок — в `docs/strategy-tuning-sql.md`.

## 10. Шпаргалка «изменил → проверь»

```
изменил _ENABLED=true  → проверь, что strategy_eval_total{strategy=...} > 0 за час
изменил _SHADOW_ONLY=false + PROMOTION_ALLOWED=true → проверь promotion_reviews.eligible
изменил threshold ↓ → жди роста shadow rows; следи за reversal_15m_ratio
изменил worker INTERVAL ↓ → следи за upstream API rate-limit ошибками
изменил TELEGRAM_USER_FLOW=true → ровно одну стратегию за раз; 48h мониторинг
```
