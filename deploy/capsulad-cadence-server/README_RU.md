# Инструкция по настройке сервера Cadence для `capsulad`

Вам потребуется Docker, немного терпения и эта папка (capsulad-cadence-server) на компьютере, на котором будет развёрнут сервер.

## Развёртывание сервера Cadence
Соберите Docker-образы `cadence-server` и `cadence-dashboard` из исходников:
```sh
git clone https://git.shino.boo/devilreef/cadence
cd cadence && mkdir ../cadence-src && git archive HEAD | tar -x -C ../cadence-src
docker build -f server/Dockerfile --build-arg VERSION=$(git rev-parse --short HEAD) -t cadence-server:local ../cadence-src
docker build -f deploy/dashboard/Dockerfile -t cadence-dashboard:local .
```

После успешной сборки не забудьте удалить папки cadence/ и cadence-src/.

Поменяйте значения в файле .env в соответствии с вашим сетапом (если не уверены, просто поменяйте значение CADENCE_HOST на локальный IP-адрес сервера), затем выполните:
```sh
docker compose run setup
```

После того, как сгенерируются сертификаты, запустите серверный стэк:
```sh
docker compose up -d
```

Затем установите пароль администратора для входа в веб-интерфейс:
```sh
sh ./setup/make-password.sh
```

Затем откройте `https://(CADENCE_HOST):(CADENCE_PORT)/` в вашем браузере, войдите с вашим паролем и выполните первоначальную настройку.

## Обмен корневыми сертификатами
У `capsulad` есть свой самоподписанный TLS-сертификат, и у `cadence-server` тоже есть такой сертификат. Чтобы всё работало корректно, они должны доверять сертификатам друг друга. Поэтому:

### Добавьте сертификат `cadence-server` в `capsulad`:
Возьмите файл `./data/ca.crt` в папке с Cadence, откройте веб-интерфейс `capsulad`, откройте раздел "Файлы" ("Files") и загрузите взятый файл в `/assets/cadence/ca.crt`.

### Добавьте сертификат `capsulad` в `cadence-server`:
Откройте раздел "Доступ" ("Access") в веб-интерфейсе `capsulad`, нажмите "Скачать корневой сертификат" ("Download CA certificate") и положите скачанный файл в папку `./data/ca-certs/` в Cadence.

## Подключение к `capsulad`
Установите приложение "Клиент Cadence" ("Cadence Client") на ваше устройство `capsulad`, откройте настройки приложения и следуйте инструкциям для настройки подключения.

# Обслуживание сервера Cadence

## Обновление сервера Cadence
Проще некуда, просто выполните эти команды еще раз:
```sh
# Снова собрать образы
git clone https://git.shino.boo/devilreef/cadence
cd cadence && mkdir ../cadence-src && git archive HEAD | tar -x -C ../cadence-src
docker build -f server/Dockerfile --build-arg VERSION=$(git rev-parse --short HEAD) -t cadence-server:local ../cadence-src
docker build -f deploy/dashboard/Dockerfile -t cadence-dashboard:local .

# Развернуть свежий стэк
docker compose up -d
```

Не забудьте удалить папки cadence/ и cadence-src/.

## Замена `CADENCE_HOST` или `CADENCE_PORT`
После того, как вы измените значения, нужно заново сгенерировать сертификаты:
```sh
docker compose stop
docker compose run setup
docker compose start
```

Затем возьмите файл `./data/ca.crt` и заново загрузите его в `capsulad`.