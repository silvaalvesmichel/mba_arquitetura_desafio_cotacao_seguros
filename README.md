# Prumo Cota: a fronteira com as seguradoras, protegida

Entrega do desafio "Fundamentos de Arquitetura de Solução". O enunciado original está em
[`docs/enunciado.md`](docs/enunciado.md).

## 1. Onde está cada coisa

- **SAD:** [`docs/sad.md`](docs/sad.md)
- **Evidências:** [`docs/evidencias/README.md`](docs/evidencias/README.md), com as sete do enunciado, cada uma com comando ou consulta, artefato e legenda
- **Enunciado:** [`docs/enunciado.md`](docs/enunciado.md)

## O coração da solução

Protegi a chamada da `quotation-api` a cada seguradora, que antes saía sem limite de espera e derrubava a
cotação inteira quando uma das três falhava. Cada chamada agora tem teto de 2 s e passa por um circuit
breaker próprio da parceira, que abre depois de 5 falhas consecutivas e, aberto, não deixa a requisição
sair; cada proposta fica 300 s num cache Redis cuja chave começa pela corretora; e a parceira que não
responde vira um item de `unavailable` numa resposta 200 marcada como `degraded`, em vez de um 502. Na
carga padrão, medida no mesmo runner, o sucesso foi de 60% para 100% e o p95 de 8,00 s para 206 ms; com o
cache vazio e 50 requisições em voo, que é o pior caso, o p95 ficou em 3,87 s. O que isso custou: a
corretora pode receber uma cotação sem a seguradora mais barata e precisa olhar o campo `degraded`; uma
proposta pode ter até 300 s de idade; o breaker é código próprio, que a Prumo mantém; e a agregação
continua em série, então a primeira cotação de um risco ainda leva cerca de 1,9 s.

## 2. Como subir e reproduzir

Precisa de Docker com Compose v2 e de Go 1.25.

```bash
make down && make reproduce
```

Sobe o ambiente e roda a carga padrão (10 de baseline, depois 200 com 50 em voo). A `quotation-api` já
sobe protegida, com os valores do `docker-compose.yml`:

| Variável | Valor | O que é |
|---|---|---|
| `PARTNER_TIMEOUT_MS` | `2000` | teto de espera por parceira |
| `BREAKER_FAILURE_THRESHOLD` | `5` | falhas consecutivas que abrem o circuito |
| `BREAKER_OPEN_SECONDS` | `10` | tempo aberto antes de uma sonda |
| `REDIS_ADDR` | `redis:6379` | endereço do cache |
| `CACHE_TTL_SECONDS` | `300` | vida de uma proposta no cache (acima de 86400 a API não sobe) |
| `CACHE_TIMEOUT_MS` | `100` | teto de um comando Redis |

As variações que sustentam os gráficos do breaker, com os perfis das parceiras intocados:

```bash
make load ARGS="-baseline 0 -requests 120 -concurrency 1 -distinct 120 -duration 10m"
make load ARGS="-tenant corretora-b -baseline 0 -requests 200 -concurrency 50 -distinct 200 -duration 10m"
go run ./cmd/loadgen -tenant corretora-b -baseline 0 -requests 200 -concurrency 50 -distinct 200 -duration 10m
```

Testes:

```bash
make test
make smoke
```

Uma cotação à mão, para ver o contrato:

```bash
curl -s localhost:8080/quotes \
  -H 'Content-Type: application/json' \
  -H 'X-Tenant-Id: corretora-a' \
  -d '{"driver":{"document":"12345678901","birth_year":1985},
       "vehicle":{"plate":"ABC1D23","model":"Onix 1.0","year":2020,"value_cents":7500000},
       "coverage":"comprehensive"}'
```

### Limitação documentada: as evidências vieram de um runner do GitHub Actions

A máquina em que desenvolvi é Windows sem permissão de administrador: não tem Docker (depende de WSL2,
que só o suporte habilita) nem `make`. Em vez de contornar, medi tudo num runner do GitHub (Linux, 4 vCPU,
15 GiB, Docker 29.4.2, Compose v5.1.3, Go 1.25), com o workflow
[`.github/workflows/evidencias.yml`](.github/workflows/evidencias.yml), que roda os mesmos comandos do
enunciado e grava os artefatos na própria branch:

- o **antes** foi medido na branch `verify/antes`, que é o código original (commit `cbc3c1e`) mais o
  workflow, antes de qualquer alteração no código;
- o **depois** foi medido na branch `verify/depois`, commit `1764e80`, com o mesmo código desta `main`.

As duas execuções rodaram no mesmo tipo de runner, então o antes e o depois são comparáveis entre si. O que
não foi feito por causa disso: abrir a UI do Jaeger e do Prometheus num navegador. Os traces são o JSON
exportado pela API do Jaeger, e os gráficos são PNG desenhados a partir de `/api/v1/query_range` do
Prometheus, com a consulta e a janela escritas dentro da imagem e os pontos brutos no `.json` ao lado.
Localmente rodei `go test ./...` e `go test ./cmd/partner-mock -run Feasibility -v`, que é o que
`make test` e `make smoke` executam. O workflow e o script de coleta não fazem parte do enunciado.

## 3. O que foi implementado e o que ficou como proposta

Implementado:

| O quê | Onde |
|---|---|
| Timeout de 2 s por parceira; o estouro conta como falha para o breaker | `internal/partner/client.go` |
| Circuit breaker com os estados `Closed`, `HalfOpen` e `Open`, um por parceira; aberto, a parceira não é chamada | `internal/breaker/breaker.go`, ligado em `internal/partner/client.go` |
| Cache Redis por parceira, TTL de 300 s, nunca além da validade que a parceira declara | `internal/quotation/service.go`, `internal/quotation/cache.go`, `internal/cache/redis.go` |
| Fallback: resposta parcial, com a degradação no contrato | `internal/quotation/service.go`, `internal/quotation/request.go` |
| Parâmetros e boot | `internal/platform/config.go`, `cmd/quotation-api/main.go`, `docker-compose.yml` |
| Testes determinísticos, sem `sleep` | `internal/breaker/breaker_test.go`, `internal/partner/client_resilience_test.go`, `internal/quotation/resilience_test.go`, `internal/platform/config_resilience_test.go` |

**A chave de cache, por extenso:**

```
quote:v1:{tenant_id}:{partner}:{sha256(JSON do pedido normalizado), em hexadecimal}
```

`{tenant_id}` é a corretora do cabeçalho `X-Tenant-Id`; `{partner}` é o nome da parceira; o hash cobre
documento, ano de nascimento, placa, modelo, ano, valor do veículo e cobertura. A mesma placa cotada por
duas corretoras gera duas chaves, e o teste `TestCacheNeverCrossesBrokers` prende isso. Invalidação: só
por TTL; todo `SET` vai com expiração e o Redis descarta a entrada sozinho.

**O que mudou no contrato de `POST /quotes`:**

- `degraded` (booleano, sempre presente): `true` quando falta ao menos uma parceira;
- `unavailable` (só na resposta degradada): lista de `{partner, reason}`, com `reason` igual a
  `circuit_open`, `timeout` ou `error`;
- em cada proposta: `source` (`partner` ou `cache`), `fetched_at` (instante da consulta que produziu o
  prêmio) e `age_seconds`;
- sem nenhuma proposta para apresentar, continua o 502 `partner insurer unavailable` que já existia.

Um exemplo real de resposta degradada está em
[`docs/evidencias/depois/resposta-degradada.json`](docs/evidencias/depois/resposta-degradada.json).

**Uma limitação que encontrei no próprio repositório.** Dois testes que vieram prontos
(`TestOnePartnerDownBringsDownTheWholeRequest` e `TestQuotesResponds502WithTheNameOfThePartnerThatWentDown`)
exigem o comportamento que o enunciado manda mudar: uma parceira fora derruba a requisição. Para não
quebrar o que já passava, `quotation.NewService(partners, quoter)` e `partner.NewClient()` sem opções
continuam se comportando como antes, e cache, resposta parcial, timeout e breaker são ligados por opções
que o `cmd/quotation-api/main.go` sempre passa. Um terceiro teste, `TestQuoteCallsThePartnersSerially`,
fixa a agregação em série, e por isso a paralelização (bônus) não foi implementada.

Ficou como proposta, defendido no SAD e sem código:

- registro de auditoria por 5 anos, não regravável (seções 5.3 e 8.2 do SAD);
- hospedagem em nuvem pública em região no Brasil (seção 5.3);
- regras de alerta no Prometheus: as consultas da seção 6.1 usam métricas que existem, mas não há
  Alertmanager nem arquivo de regras no repositório;
- paralelização da agregação (seção 4.5);
- HMAC com segredo no lugar do SHA-256 na chave de cache (seção 4.2);
- região secundária em *pilot light* (seção 7.4).

Os quatro limites conhecidos que a entrega não resolve estão na seção 4.4 do SAD.

## 4. Métricas e atributos criados

| Métrica (OpenTelemetry) | No Prometheus | Tipo | Atributos |
|---|---|---|---|
| `quotation.breaker.state` | `quotation_breaker_state` | gauge: 0 fechado, 1 meio aberto, 2 aberto | `partner` |
| `quotation.breaker.transitions` | `quotation_breaker_transitions_total` | contador | `partner`, `from`, `to` |
| `quotation.cache.requests` | `quotation_cache_requests_total` | contador | `result` (`hit`, `miss`, `error`), `partner`, `tenant_id` |
| `quotation.partner.duration` | `quotation_partner_duration_seconds` | histograma, em segundos | `partner`, `outcome` (`success`, `error`, `timeout`, `canceled`) |
| `quotation.responses` | `quotation_responses_total` | contador | `outcome` (`complete`, `degraded`, `unavailable`), `tenant_id` |

A latência por parceira é métrica nova, e não o reaproveitamento de `http_client_request_duration_seconds`:
ela só registra chamadas que de fato saíram e separa o estouro de timeout do erro.

Hit rate em PromQL:

```promql
sum(rate(quotation_cache_requests_total{result="hit"}[5m])) / sum(rate(quotation_cache_requests_total[5m]))
```

Marcações de trace, no span `partner.quote` (um por parceira, aberto em `internal/quotation/service.go`):

| Atributo | Quando |
|---|---|
| `quotation.breaker.short_circuited = true`, mais o evento `partner call short-circuited: circuit open` | o breaker impediu a chamada; não existe span `HTTP POST` filho |
| `quotation.cache.result` = `hit`, `miss` ou `error` | resultado da consulta ao cache; com `hit` não existe span `HTTP POST` filho |
| `partner`, `tenant_id` | sempre |

No span de servidor `POST /quotes`: `quotation.degraded` e `quotation.cache.hits`.

Nenhum atributo de span ou de métrica carrega CPF, placa ou `quote_id`: conferido por teste
(`TestSpansMarkCacheHitsAndCarryNoPersonalData`) e nos traces da execução
([`docs/evidencias/depois/atributos-de-span.txt`](docs/evidencias/depois/atributos-de-span.txt)).

## 5. O que eu faria diferente com mais tempo

- **Mediria em Docker local**, com a UI do Jaeger e do Prometheus abertas, em vez de depender do runner.
- **Capturaria `quotation_responses_total` como evidência.** O relatório do `loadgen` conta a resposta
  degradada como sucesso, então os 100% não dizem quantas cotações saíram incompletas.
- **Paralelizaria a agregação**, combinando com quem escreveu o teste que fixa a série: o teto do p95 com
  cache vazio cairia de 3,9 s para 2,0 s.
- **Limitaria as chamadas simultâneas por parceira.** Na variação 2 o circuito da `partner-degrading`
  abriu porque 50 requisições chegaram nela de uma vez; um limite de 5 evitaria o afundamento em vez de
  reagir a ele.
- **Validaria o pressuposto de recotação** (45% em 300 s) com tráfego real antes de assinar a conta da
  seção 8, porque é o único número que pode inverter o veredito.
- **Trocaria o hash da chave por HMAC**, para que o acesso ao Redis não permita testar CPFs por força bruta.
