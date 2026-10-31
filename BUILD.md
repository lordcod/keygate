Create version
```
docker buildx build --platform linux/amd64 --load -t cr.yandex/crp7sjuopuil9h33ujci/keygate:latest .
```


Push version
```
docker push cr.yandex/crp7sjuopuil9h33ujci/keygate:latest
```

Image
```
image: cr.yandex/crp7sjuopuil9h33ujci/keygate:latest
```

Pull server
```
docker compose pull keygate
docker compose up -d --force-recreate keygate
docker compose logs --since=2m keygate
```