# Несколько репозиториев на один run — план реализации

Статус: план, не реализовано. Решения по открытым вопросам приняты (§11).

## 1. Цель

Сейчас run принимает один `repo`: он клонируется в `/workspace` и становится
рабочей директорией агента. Нужно принимать **список** репозиториев:

- `repos[0]` — основной: клонируется в `/workspace`, это cwd агента.
- `repos[1..]` — дополнительные: клонируются до старта агента в `/repos/<name>`,
  агенту сообщается, где они лежат, и, где CLI умеет, они подключаются как
  дополнительные рабочие директории (`--add-dir`).
- **Во всех** репозиториях агент может менять файлы. Для каждого репозитория, где
  есть изменения, entrypoint делает commit, push в ветку run'а и (если
  `create_pr`) создаёт или обновляет PR/MR — так же, как сегодня для одного.
- Число репозиториев на run ограничено настройкой, которая меняется **только в
  веб-UI** (страница Settings), по умолчанию **10**.

Вне цели: чат-интеграции и alert receivers (у них по-прежнему один `repo_url`),
текст ответов в чат (в нём остаётся PR основного репозитория).

## 2. Что есть сейчас (точки, которые придётся трогать)

| Слой | Где | Что про репозиторий |
|---|---|---|
| Запрос | `backend/restapi/runs.go`, `backend/mcp/tools.go` (`repo`, `base_branch`, `target_branch`, `create_pr`) | одна строка |
| Admission | `backend/run/spec.go` (`SubmitRequest.RepoURL`, `validate`, `renderRepo`, `ProviderFor`) | один `RepoSpec` |
| Спека | `api/run/v1/runspec.go` `RenderedRunSpec.Repo RepoSpec` | одно поле, едет в lease и в CRD |
| Отчёт | `api/run/v1/completion.go` `CompletionReport.Repo *RepoResult` | один результат |
| Хранилище | `runs.repo_url/…/target_branch` (0003), `runs.pr_url/pr_number/pr_action/commit_sha/pushed`, запись в `promoteAttempt` (`backend/store/report.go`) | одни колонки |
| Токен | `backend/app/materials.go` `GitTokenMinter.MintToken(repoURL, provider, ttl)`, `gitTokenTarget` | один токен → `SecretKeyGitToken` |
| Контроллер | `controller/launcher/job.go` `env()` → `HALIPHRON_REPO_URL` и др.; 4 `emptyDir` | env одного репо |
| Docker host | `dockerhost/launcher/launcher.go` — 4 именованных тома | те же env |
| Образ | `image/entrypoint/config.go`, `git.go` (`phaseClone/Commit/Push/PR`, `r.git` всегда в `Workspace`), `agent.go` (`writeGitCredentialHelper(host)` — **один** хост), `run.go` (`r.repo RepoResult`) | один репозиторий везде |
| Промпт | `image/entrypoint/runtimes.go` `systemPrompt`, `Launch` для трёх CLI | «Work in /workspace» |
| Шаблоны | `backend/app/schedule.go` (template.repo), `deferred.go` | одна строка |
| UI-настройки | `token_retention`/`run_retention` (0014): singleton-таблица, «нет строки = дефолт», `GET/PUT /token-retention`, `SettingsPage.tsx` | образец для лимита |

## 3. Решения

### 3.1. Форма спеки: `Repo` остаётся, рядом `ExtraRepos`

```go
// RenderedRunSpec
Repo       RepoSpec        `json:"repo"`
// +optional
// +kubebuilder:validation:MaxItems=31
ExtraRepos []ExtraRepoSpec `json:"extraRepos,omitempty"`

type ExtraRepoSpec struct {
    // +kubebuilder:validation:MinLength=1
    // +kubebuilder:validation:MaxLength=2048
    URL string `json:"url"`
    // +optional
    Provider GitProvider `json:"provider,omitempty"`
    // Пусто = ветка по умолчанию у remote (origin/HEAD).
    // +optional
    // +kubebuilder:validation:MaxLength=255
    BaseBranch string `json:"baseBranch,omitempty"`
    // Path — имя директории под MountRepos, уникально в пределах run'а.
    // +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`
    Path string `json:"path"`
    // +optional
    // +kubebuilder:validation:Minimum=0
    CloneDepth int32 `json:"cloneDepth,omitempty"`
}
```

Общие для всех репозиториев поля берутся из `Repo`: `TargetBranch` (одна ветка
`haliphron/run-<id>` во всех репозиториях), `CreatePR`, `Submodules`, `LFS`.
Отдельных ручек на каждый репозиторий не вводим.

Почему не превращать `Repo` в список:

- поле аддитивное — старые спеки в `runs.spec` и существующие CR остаются валидными,
  иммутабельность спеки (CEL-правило в CRD, триггер в store) не задета;
- `drift_test.go` сравнивает OpenAPI и CRD по ключам — добавить ключ проще, чем
  менять тип существующего;
- колонки `runs.repo_*`, `runs.pr_*`, список run'ов, чат-ответы продолжают
  описывать основной репозиторий без изменений.

`MaxItems=31` — жёсткий потолок схемы (итого 32 с основным). Структурной схеме CRD
нужен предел; настраиваемый лимит (§3.5) не может быть выше него, это проверяет
`CHECK` в БД и валидация `PUT`.

**Глубина клона дополнительных по умолчанию — 1** (shallow): десять полных клонов
легко выбьют `ephemeralStorage`. Запрос может указать `depth: 0` для полного клона.
Push из shallow-клона в тот же `origin` работает: сервер знает базовый коммит,
новой ветке нужны только новые коммиты. (Ошибка `shallow update not allowed`
возникает при push в remote, где базы нет, — не наш случай.) Основной репозиторий
клонируется как сейчас.

### 3.2. Раскладка в поде: отдельный том `/repos`

```
/workspace            ← repos[0], как сейчас (cwd агента)
/repos/<path>         ← repos[1..]
```

Новая константа `runv1.MountRepos = "/repos"` и пятый `emptyDir` (k8s) / пятый
именованный том (docker host).

Альтернатива — `/workspace/.haliphron/repos/<path>` (уже исключено из git, тома
не нужно) — отвергнута: вложенные репозитории внутри рабочего дерева основного
путают агента и инструменты («какой это `src/main.go`?» — ровно то, от чего
защищает комментарий к `MountWorkspace`), `rg`/`grep -r` начинают обходить чужие
деревья, `--add-dir` на поддиректорию cwd ничего не значит.

`path` по умолчанию — basename URL без `.git` (`https://github.com/acme/lib.git` →
`lib`); при совпадении — суффикс `-2`, `-3`. Запрос может задать `name` явно.
`.`/`..`/имена со `/` отвергает паттерн.

### 3.3. Токены: несколько хостов, несколько провайдеров

Сейчас один токен и helper, привязанный к одному хосту. Возможны: тот же хост
(типично), другой хост того же провайдера (GitHub Enterprise + github.com),
другой провайдер (GitHub + GitLab).

- Новый ключ per-run Secret `runv1.SecretKeyGitTokens = "git-tokens.json"`:
  `{"<host>": {"provider": "github", "token": "..."}}` — запись на каждый хост из
  `repo` + `extraRepos`. `SecretKeyGitToken` остаётся и содержит токен основного
  репозитория (совместимость со старым образом).
- `GitTokenMinter`: вызов на каждую уникальную пару `(host, provider)`; порт
  расширяется до `MintToken(ctx, repoURLs []string, provider, ttl)` — installation
  token GitHub App можно выпустить сразу на список репозиториев одной инсталляции.
  `StoredGitToken` по сути не меняется (он и так выбирает по провайдеру).
- Образ (`phaseAuth`): разбирает `git-tokens.json`, кладёт каждый токен в
  `RunPrivate/git-tokens/<host>` (0400); helper отвечает через
  `case "$asked" in host1|host2) …` **только** хостам из спеки. Правило «helper
  не отвечает хосту, который назвал сам репозиторий» (marketplaces) сохраняется.
- Окружение forge-CLI (`gitEnv`) становится функцией репозитория:
  `GH_TOKEN`/`GL_TOKEN` берутся из токена его хоста, а не из единственного.
- Все токены регистрируются в redactor.
- Известное ограничение: два репозитория на одном хосте, которым нужны разные
  токены (разные GitHub App инсталляции), не поддерживаются. Расширение — ключ
  `host/owner` и `credential.useHttpPath=true`.

### 3.4. Как агенту сообщают о дополнительных репозиториях

1. **systemPrompt** (`runtimes.go`) — блок, если `ExtraRepos` не пуст:
   ```
   Additional repositories are checked out next to your working directory:
   - /repos/lib — https://github.com/acme/lib (base branch main)
   - /repos/infra — https://gitlab.example.com/ops/infra (base branch release)
   The branch haliphron/run-… already exists and is checked out in every one of them.
   Change any of them as the task requires; do not create or rename branches and
   do not push — changes in each repository are committed, pushed and proposed
   for review for you.
   ```
2. **CLI**:
   - claude-code: `--add-dir /repos/<path>` на каждый — иначе запись вне cwd
     упирается в permission prompt, которого в `-p` нет;
   - codex: `--add-dir /repos/<path>` на каждый — sandbox `workspace-write`
     запрещает запись вне cwd. **Проверить**, что флаг есть в версии,
     закреплённой в `image/Dockerfile`; если нет — поднять версию;
   - opencode: доступ вне проекта управляется `permission.external_directory`;
     при рендере конфигурации (`opencode.go`) разрешить `"/repos/**"`.
     **Проверить** поведение в headless-режиме на закреплённой версии.
3. **Плагины и настройки**: `.claude/settings*.json`, MCP-конфиги, marketplaces
   читаются **только** из основного репозитория. Дополнительные — такой же
   недоверенный контент и не добавляют плагины или MCP-серверы. Фиксируется тестом.

### 3.5. Лимит числа репозиториев — только из веб-UI

Не env и не Helm: значение хранится в БД и меняется со страницы Settings, по
образцу `token_retention`.

- Миграция `0023_run_limits.sql`:
  ```sql
  CREATE TABLE run_limits (
    tenant_id          uuid        PRIMARY KEY DEFAULT '00000000-0000-0000-0000-000000000000',
    max_repos_per_run  smallint    NOT NULL CHECK (max_repos_per_run BETWEEN 1 AND 32),
    updated_by         text        NOT NULL,
    updated_at         timestamptz NOT NULL DEFAULT now()
  );
  ```
  Нет строки — действует встроенный дефолт `run.DefaultMaxRepos = 10`.
  Таблица названа `run_limits`, а не `repo_limits`, — следующие лимиты run'а
  ложатся в неё же колонками.
- `backend/store/limits.go`: `RunLimits(ctx)`, `PutRunLimits(ctx, limits, by)`.
- `backend/app/limits.go`: те же use cases, проверка `1..32`
  (`run.MaxReposCeiling = 32`, равен потолку схемы).
- REST: `GET /run-limits` (`ScopeRunsRead` — форме создания расписания нужно знать
  лимит) и `PUT /run-limits` (`ScopeAdmin`), тело
  `{"max_repos_per_run": 10}`. Ответ GET содержит и `default_max_repos_per_run`,
  чтобы UI показывал, что будет без сохранённого значения.
- Admission читает лимит из store на каждый submit (singleton по первичному
  ключу — дёшево; без кэша все реплики сразу видят новое значение).
  `len(repos) > limit` → 422 `invalid("repos", "%d repositories exceeds the limit
  of %d")` — лимит назван в сообщении, вызывающий часто модель.
- Уменьшение лимита не трогает уже принятые run'ы: спека заморожена при admission.
- UI: карточка «Run limits» на `SettingsPage.tsx` с полем «Max repositories per
  run» (1–32), хуки `useRunLimits`/`usePutRunLimits`, mock в
  `frontend/mock/server.mjs`.

## 4. Git-фазы по нескольким репозиториям

Внутреннее представление в образе: `r.repos []*repoWork`, где `[0]` — основной:

```go
type repoWork struct {
    Dir, URL, Path, BaseBranch string
    Provider runv1.GitProvider
    Host     string
    Result   runv1.RepoResult
    committed bool // этой попыткой — то, о чём сегодня спрашивает outcomes[commit]
}
```

`r.git(ctx, args...)` → `r.gitIn(ctx, w, args...)` (Dir и env по репозиторию).
`safeDirectoryEnv` расширяется на каждый `/repos/<path>`
(`GIT_CONFIG_COUNT` = 1 + N).

| Фаза | Поведение |
|---|---|
| `clone` | основной, как сейчас |
| `clone-extra` (новая, сразу после `clone`) | для каждого: clone `[--depth N] [--branch B]` в `/repos/<path>`, submodules/LFS как у основного, `checkout -B <target>`; `ExtraCloneFailed` через `gitFailure` |
| `commit` | для каждого репозитория: `status` → `add -A` → `commit`, затем `diff --quiet origin/<base или HEAD> HEAD`. Фаза `skipped`, только если изменений нет **ни в одном** |
| `push` | только репозитории с изменениями; `--force-with-lease` в `refs/heads/<target>` |
| `pr` | только запушенные; сначала дополнительные, потом основной (см. ниже) |

**Связь PR между собой.** Дополнительные PR создаются первыми: в их описании —
ссылка на основной репозиторий, ветку и run. Описание PR основного репозитория
перечисляет PR дополнительных («This change spans N repositories: …»).
create-or-update сохраняется: на повторной попытке существующий PR обновляется,
а не дублируется.

**Частичный отказ.** Фаза commit/push/pr проходит **все** репозитории, даже если
один упал: 403 в одном не должен оставлять незапушенной работу в остальных.
Итог фазы:

- все успешны или пропущены → успех;
- есть отказы → фаза падает после обхода всех; код — `ExitConfig`, если **все**
  отказы постоянные (`isPermanentGitRefusal`), иначе `ExitGit` (ретрай);
- результат каждого репозитория попадает в отчёт в любом случае, так что уже
  открытые PR видны даже у упавшего run'а.

Ретрай ведёт себя как сегодня для одного репозитория: при resume модель не
вызывается, клоны свежие, отправлять нечего. Это существующее свойство, мульти-репо
его не меняет.

## 5. Отчёт и хранение результатов

- `api/run/v1/completion.go`:
  ```go
  // CompletionReport
  // +optional
  // +kubebuilder:validation:MaxItems=31
  ExtraRepos []ExtraRepoResult `json:"extraRepos,omitempty"`

  type ExtraRepoResult struct {
      Path       string `json:"path"`
      RepoResult `json:",inline"`
  }
  ```
  `Repo` остаётся результатом основного.
- Миграция (в той же `0023` или отдельной `0024`): `runs.extra_repo_results jsonb`.
- `promoteAttempt` (`backend/store/report.go`): в той же транзакции читает текущее
  значение и сливает по `path` поле-за-полем по правилу `COALESCE`, как для
  колонок основного: отчёт поздней попытки без PR не стирает PR ранней.
- CR status и состояние docker host не меняются (`PRURL` — основного): extras им
  не нужны.

## 6. Публичный API

### REST `POST /runs` (snake_case)

```json
{
  "prompt": "...",
  "repos": [
    {"url": "https://github.com/acme/app", "base_branch": "main"},
    {"url": "https://github.com/acme/lib"},
    {"url": "https://gitlab.example.com/ops/infra", "base_branch": "release",
     "name": "infra", "depth": 0}
  ],
  "create_pr": true
}
```

- `repos[0]` эквивалентен `repo` + `base_branch`; `target_branch` и `create_pr`
  действуют на все репозитории.
- `repo` остаётся. `repo` и `repos` вместе → 422; `base_branch` вместе с `repos` → 422.
- Валидация каждого элемента: длина ≤ 2048, схема — та же, что принимается
  сегодня для `repo` (сверить с `ProviderFor`), **без userinfo** в URL (креды в URL
  попадут в CR, логи и системный промпт), без дублей (нормализованный URL),
  уникальный `name`, `name` у `repos[0]` не допускается (он всегда `/workspace`).
- `GET /runs/{id}`: новое поле `repos` — массив
  `{url, path, base_branch, target_branch, pushed, commit_sha, pr_url, pr_number, pr_action}`,
  основной первым (из колонок), дополнительные из спеки + `extra_repo_results`.
  Старые `repo`, `pr_url` и т.д. остаются. `GET /runs` не меняется.

### MCP

- `run_agent` / `schedule_agent`: аргумент `repos` — массив объектов
  `{url, base_branch?, name?, depth?}`. Массив объектов, а не
  `oneOf string|object`: часть MCP-клиентов плохо переносит `oneOf`. Та же
  `SubmitRequest`, никаких отдельных правил.
- `get_run_result` (`backend/mcp/tools.go:413`): перечисляет PR всех репозиториев,
  не только основного — родительскому агенту они нужны.

### Шаблоны

- Расписания: `template.repos` в jsonb (миграция не нужна — template и есть
  request, см. комментарий в 0010); `schedule.go` переносит поле в обе стороны.
- Deferred: `deferred.go` копирует `ExtraRepos` вместе с `RepoURL`.
- Чат-интеграции, alert receivers, чат-ответы — **без изменений**.

## 7. Совместимость версий

Старый контроллер, получив lease с `extraRepos`, тихо проигнорирует поле, и run
пройдёт без дополнительных репозиториев, а значит, и без их PR — молчаливая потеря,
хуже явного отказа. Поэтому:

- lease-путь (`backend/app/lease.go`) не отдаёт run с непустым `ExtraRepos`
  кластеру, чья `ControllerVersion` меньше версии релиза с этой функцией; такой run
  ждёт подходящий кластер. Проверить, есть ли уже фильтр лиза по версии, иначе
  добавить.
- Старый образ, закреплённый ролью (`role.Image`), не прочитает
  `HALIPHRON_EXTRA_REPOS`. Защититься со стороны образа нельзя — описывается в
  CHANGELOG и `role-spec.md`.

## 8. Контракты и генерация

| Что | Изменение |
|---|---|
| `api/run/v1/runspec.go` | `ExtraRepoSpec`, `RenderedRunSpec.ExtraRepos` |
| `api/run/v1/completion.go` | `ExtraRepoResult`, `CompletionReport.ExtraRepos` |
| `api/run/v1/runtime.go` | `EnvExtraRepos = "HALIPHRON_EXTRA_REPOS"` (JSON `[{url, provider, baseBranch, path, cloneDepth}]` — не секрет, те же данные в CR), `MountRepos`, `SecretKeyGitTokens`, `RuntimePhaseCloneExtra = "clone-extra"` |
| генерация | `make generate chart-gen`, `make verify` |
| `api/cluster/v1/openapi.yaml` | `extraRepos` в спеке и в completion; ключ `git-tokens.json` в materials |
| `api/runtime/v1/openapi.yaml` | новая фаза, `extraRepos` в отчёте пода |
| `docs/contracts/agent-runtime.md` | env-таблица, раскладка ФС, фаза, Secret-ключ, git-фазы по нескольким репозиториям и правило частичного отказа, helper только для хостов спеки — `runtime_test.go` парсит таблицы |
| `docs/contracts/agentrun-crd.md` | поле, потолок 31 |
| `docs/contracts/cluster-api.md` | `git-tokens.json`, `extraRepos` в отчёте, гейт по версии контроллера |
| `docs/contracts/docker-host.md` | пятый том и его очистка |
| `docs/contracts/run-store.md` | `run_limits`, `runs.extra_repo_results` и правило слияния |

Отдельная фаза `clone-extra`, а не продолжение `clone`: в UI видно, что упало,
и `completedPhases` работают пофазно.

## 9. Реализация по слоям

1. **api/** — типы, константы, маркеры; генерация; OpenAPI обоих контрактов.
2. **db/** — `0023_run_limits.sql` (+ `extra_repo_results`).
3. **backend/store** — `limits.go`; слияние `extra_repo_results` в `promoteAttempt`;
   чтение для `GET /runs/{id}`.
4. **backend/run** — `SubmitRequest.ExtraRepos []RepoRequest`; `validate` (лимит
   передаётся параметром, дубли, userinfo, имена); `renderExtraRepos` (provider
   через `ProviderFor`, имя по умолчанию, `depth=1` по умолчанию);
   `DefaultMaxRepos`, `MaxReposCeiling`.
5. **backend/app** — `limits.go`; admission читает лимит; `materials.go`
   (`gitTokenTargets` → `git-tokens.json` + `git-token`); `lease.go` (гейт версии);
   `schedule.go`, `deferred.go`.
6. **backend/restapi, backend/mcp** — `repos` в запросе и ответе,
   `GET/PUT /run-limits`, схемы `run_agent`/`schedule_agent`, `get_run_result`.
7. **controller/launcher/job.go** — `HALIPHRON_EXTRA_REPOS`, пятый `emptyDir`.
   `controller/materialize` — убедиться, что `git-tokens.json` проходит и
   учитывается в подсчёте размера Secret.
8. **dockerhost/launcher** — пятый том (`VolumeNames` и очистка подхватят его;
   проверить `reconcile`).
9. **fake/controller/materialize.go** — та же env и том. **fake/controlplane** —
   принимает и хранит `extraRepos` отчёта.
10. **image/entrypoint** — `config.go` (разбор, `ExitConfig` на невалидном),
    `agent.go` (helper на несколько хостов), `git.go` (`repoWork`, `gitIn`,
    `phaseCloneExtra`, циклы в commit/push/pr, агрегирование отказов, тексты PR),
    `report.go` (`ExtraRepos` в отчёте), `runtimes.go`/`opencode.go` (промпт и
    флаги).
11. **frontend/** — `types.ts`; `RunDetailPage` — таблица репозиториев с ветками,
    коммитами и PR; `RunsPage` — «+N» в колонке Repository; формы расписаний —
    редактор списка с лимитом из `GET /run-limits`; `SettingsPage` — карточка
    «Run limits»; `mock/server.mjs`.
12. **Документация** — `rest-api.md`, `mcp-tools.md`, `role-spec.md`, how-to
    «Run across several repositories», `CHANGELOG.md`.

## 10. Тесты (имена — предложения)

backend/run, backend/app:
- `TestTheFirstRepositoryIsTheWorkingRepositoryAndTheRestAreExtra`
- `TestARequestWithMoreRepositoriesThanTheLimitIsRefusedNamingTheLimit`
- `TestWithoutASavedLimitTenRepositoriesAreAllowed`
- `TestALimitSavedInTheUIAppliesToTheNextAdmission`
- `TestRepoAndReposTogetherAreRefused`
- `TestARepositoryURLCarryingCredentialsIsRefused`
- `TestDuplicateExtraRepositoriesAreRefused`
- `TestExtraRepositoryNamesDefaultToTheURLBasenameAndAreMadeUnique`
- `TestATokenIsMintedForEveryHostTheRunClones`
- `TestARunWithExtraRepositoriesIsNotLeasedToAnOlderController`

test/store, test/backend:
- `TestARunLimitOutsideOneToThirtyTwoIsRefused`
- `TestALaterReportWithoutAPullRequestKeepsTheExtraRepositorysEarlierOne`
- `TestPuttingRunLimitsNeedsTheAdminScope`

controller / dockerhost:
- `TestExtraRepositoriesReachThePodAsOneVariable`
- `TestTheReposVolumeIsMountedAndCleanedUp` (docker host)

image (против `fake/controlplane`):
- `TestExtraRepositoriesAreClonedBesideTheWorkspaceOnTheRunsBranch`
- `TestTheCredentialHelperAnswersEveryHostOfTheRunAndNoOther`
- `TestEveryRepositoryWithChangesIsCommittedPushedAndProposed`
- `TestARepositoryWithoutChangesIsNotPushed`
- `TestARefusedPushInOneRepositoryDoesNotStopTheOthers`
- `TestOnlyPermanentRefusalsInEveryRepositoryAreAConfigFailure`
- `TestThePrimaryPullRequestListsTheOthers`
- `TestAnExistingPullRequestInAnExtraRepositoryIsUpdatedNotDuplicated`
- `TestExtraRepositoriesContributeNoPluginsOrMCPServers`
- `TestEveryRuntimeIsGivenTheExtraRepositoriesAsWritableDirectories`

test/contract: drift и runtime-таблицы — существующие тесты, должны пройти после
обновления документов.

## 11. Принятые решения

1. Лимит репозиториев меняется **только в веб-UI** (таблица `run_limits`), не
   через env/Helm. Дефолт 10, диапазон 1–32.
2. Дополнительные репозитории по умолчанию клонируются shallow (`depth=1`),
   `depth: 0` в запросе — полный клон.
3. Изменения в дополнительных репозиториях коммитятся, пушатся в ветку run'а и
   оформляются PR/MR так же, как в основном.
4. Чат-интеграции и alert receivers не меняются.

## 12. Что осталось проверить при реализации

- `codex exec --add-dir` в закреплённой версии codex.
- `permission.external_directory` у opencode в headless-режиме.
- Есть ли в lease-пути фильтр по `ControllerVersion`, или его нужно добавить.
- Что `controller-gen` корректно разворачивает `RepoResult` с `json:",inline"` в
  CRD/OpenAPI (иначе — явные поля в `ExtraRepoResult`).
