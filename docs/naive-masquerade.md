# Naïve Masquerade

В S-UI открой **Inbounds → Naïve** и включи **Masquerade**. Чтобы сохранить
Naïve на TCP и UDP 443, оставь порт `443`, действующий профиль TLS и пустое
поле **Network**. Пустое поле включает оба транспорта.

- **Simple** принимает URL сайта, например `http://127.0.0.1:8080` или
  `file:///var/www/naive-site`.
- **File server** раздаёт выбранную папку. Для главной страницы положи туда
  `index.html`.
- **Reverse Proxy** отправляет веб-запросы на заданный HTTP(S)-сервер.
- **Fixed response** отдаёт заданный код, заголовки и содержимое.

Обычный HTTPS-запрос открывает сайт. Naïve по-прежнему использует пользователей
и подписки панели. Неверный `CONNECT` получает 404.

## Собрать Linux-архив из ветки

Из локального репозитория отправь ветку на свой форк:

```shell
git push -u origin feat/naive-masquerade
```

На GitHub в `dactDMA/s-ui` открой **Actions → Release S-UI → Run workflow** и
выбери ветку `feat/naive-masquerade`. Когда сборка закончится, скачай артефакт
соответствующей архитектуры. Например, `uname -m` со значением `x86_64`
соответствует артефакту `s-ui-linux-amd64`. В артефакте находится архив
`s-ui-linux-amd64.tar.gz`. CLI-вариант:

```shell
gh workflow run release.yml --repo dactDMA/s-ui --ref feat/naive-masquerade
gh run list --repo dactDMA/s-ui --workflow release.yml --branch feat/naive-masquerade
gh run download RUN_ID --repo dactDMA/s-ui --name s-ui-linux-amd64 --dir dist
scp dist/s-ui-linux-amd64.tar.gz root@SERVER:/tmp/
```

Сборка применяет изменения интерфейса из `scripts/frontend-naive-masquerade.patch`
к закреплённой версии frontend. Для ручной сборки интерфейса после
`git submodule update --init --recursive` выполни `node scripts/prepare-frontend.mjs`
из корня, затем `npm ci` и `npm run build` в `frontend`.

## Обновить существующий systemd-сервис

Команды ниже рассчитаны на стандартную установку `s-ui.service` с бинарником
`/usr/local/s-ui/sui` и базой в `/usr/local/s-ui/db`. Проверь пути через
`sudo systemctl cat s-ui`, особенно если задан `SUI_DB_FOLDER`. Войди на VPS
под `root`, затем запусти:

```bash
set -e
stage=$(mktemp -d)
tar -xzf /tmp/s-ui-linux-amd64.tar.gz -C "$stage"
"$stage/s-ui/sui" -v

cd /usr/local/s-ui
backup=$(mktemp -d /root/s-ui-before-naive.XXXXXXXX)
echo "Backup: $backup"
install -m 0755 "$stage/s-ui/sui" ./sui.new
systemctl stop s-ui
cp -a ./sui ./db "$backup/"
mv -f ./sui.new ./sui
systemctl start s-ui
systemctl is-active s-ui
journalctl -u s-ui -n 30 --no-pager
```

Обновление заменяет бинарник с уже встроенным интерфейсом. Имеющаяся база,
сертификаты и systemd unit используются из текущей установки. После входа в
панель включи Masquerade и проверь сайт и оба клиентских профиля. Снаружи, с
машины, у которой `curl -V` показывает поддержку HTTP/2 и HTTP/3:

```bash
curl --http2 -fsS https://YOUR_DOMAIN/
curl --http3-only -fsS https://YOUR_DOMAIN/
```

Для следующих обновлений скачивай архивы, собранные в `dactDMA/s-ui`, и повторяй
замену бинарника. У штатного меню `s-ui` источник обновлений — upstream
`alireza0/s-ui`, и его обновление перезапишет бинарник форка.

### Откат

Подставь каталог `Backup`, который напечатала команда обновления. Для отката
используй также сохранённую базу. Она содержит настройки на момент обновления.

```bash
set -e
backup=/root/s-ui-before-naive.YOUR_SUFFIX
cd /usr/local/s-ui
test -x "$backup/sui"
test -f "$backup/db/s-ui.db"
systemctl stop s-ui
mv ./db "$backup/db.after-fork"
cp -a "$backup/db" ./db
install -m 0755 "$backup/sui" ./sui
systemctl start s-ui
```

## Docker Compose

Для существующей установки Compose собери образ из исходников форка:

```bash
git clone --branch feat/naive-masquerade --recurse-submodules https://github.com/dactDMA/s-ui.git s-ui-src
docker build -t dactdma/s-ui:naive-masquerade ./s-ui-src
```

В действующем compose-файле укажи `image: dactdma/s-ui:naive-masquerade`.
Проверь существующие тома базы и сертификатов, а также публикацию `443/tcp` и
`443/udp`, затем выполни `docker compose up -d`. Для следующей версии подтяни
ветку и подмодули, пересобери образ и снова запусти `docker compose up -d`.
