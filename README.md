# babki.my

**Учёт личных и семейных финансов с фокусом на инвестиции.** Selfhosted-first:
ваши деньги — ваша база данных. Российские брокеры и банки — первый класс:
MOEX-инструменты с купонами и НКД, замороженные активы с честной оценкой,
переносы бумаг между брокерами.

> Статус: pre-alpha, активная разработка. Не готово к использованию.

## Быстрый старт (docker compose)

```bash
git clone https://github.com/alshstf/babki.my && cd babki.my/deploy/compose
cp .env.example .env   # поменяйте POSTGRES_PASSWORD
docker compose up -d
# http://localhost:8080
```

## Лицензия

babki.my — **fair source** (не open source): [FSL-1.1-ALv2](LICENSE.md).
Можно бесплатно: селфхостить для себя/семьи/компании, форкать, изучать,
контрибьютить. Нельзя: строить на этом коде конкурирующий коммерческий
сервис. Каждый релиз через 2 года автоматически становится Apache 2.0
([DOSP](https://opensource.org/delayed-open-source-publication)).

Название «babki.my» и логотип — товарные знаки проекта, см. [TRADEMARK.md](TRADEMARK.md).

## Разработка

См. [CONTRIBUTING.md](CONTRIBUTING.md). Коротко: Go ≥1.26, Node 22, Docker.
`make test` — тесты, `make build` — полный бинарь с UI, `make ui-dev` — фронтенд с hot reload.
