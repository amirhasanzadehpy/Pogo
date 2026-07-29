import os
from pathlib import Path


BASE_DIR = Path(__file__).resolve().parent.parent

SECRET_KEY = "pogo-fixture-only-not-for-production"
DEBUG = False
ALLOWED_HOSTS = []

INSTALLED_APPS = [
    "myapp.apps.MyAppConfig",
]

MIDDLEWARE = []
ROOT_URLCONF = "sample_project.urls"

DATABASES = {
    "default": {
        "ENGINE": "django.db.backends.sqlite3",
        "NAME": os.environ.get("POGO_FIXTURE_DB", BASE_DIR / "db.sqlite3"),
    }
}

LANGUAGE_CODE = "en-us"
TIME_ZONE = "UTC"
USE_I18N = True
USE_TZ = True
DEFAULT_AUTO_FIELD = "django.db.models.BigAutoField"
