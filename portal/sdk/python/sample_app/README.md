# TPA sample app

A small web app that files and follows IDR disputes through the Python SDK (`idre_tpa.py`). Use it to see the API
working end to end, or as a starting point for your own integration. It runs on your computer only (127.0.0.1)
and has no sign-in of its own, so don't put it on a server as it is.

What it does:

- **Disputes:** everything you filed, its stage, fees owed and open document requests.
- **File a dispute:** pick a provider and the health plan, enter the claim, attach the six documents (or one
  merged PDF), and file. You get the case number, the reviewer's notes on the documents and the payment page.
- **Providers:** list them, or add one with its NPI, TIN, address and payment method on file.
- **Bulk filing:** upload `disputes.csv` or a ZIP and see each row's outcome.
- **Document requests:** attach a file and send it to the office.

## Run it

```bash
mkdir tpa-sample && cd tpa-sample
curl -O https://idre.newfire.app/sdk/python/sample_app/app.py
curl -O https://idre.newfire.app/sdk/python/sample_app/requirements.txt
curl -O https://idre.newfire.app/sdk/python/idre_tpa.py

python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt

export IDRE_CLIENT_ID=api-key-…
export IDRE_CLIENT_SECRET=…
export IDRE_ENVIRONMENT=test
python app.py
```

Then open http://127.0.0.1:5000.

- **The key:** make it in the portal (menu → API keys → New key; TPA admins). Use `IDRE_ENVIRONMENT=test` with a
  test-site key and `production` with a production key.
- **Why the virtual environment (`.venv`):** macOS has no `pip` or `python` command (only `python3`), and a
  Homebrew Python refuses to install packages system-wide. Inside `.venv`, `pip` and `python` work. In a new
  terminal, `cd` into the folder, run `source .venv/bin/activate` again and set the three `export` lines again.
- **Windows:** use `py -m venv .venv`, then `.venv\Scripts\activate`, and `set` instead of `export`.

The API key stays in your environment. It is never shown in, or sent to, the browser.

Use a key from the **test site** first. Disputes filed there are real filings on the test system, not production.
