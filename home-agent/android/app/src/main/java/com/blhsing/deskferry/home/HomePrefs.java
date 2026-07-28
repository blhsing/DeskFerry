package com.blhsing.deskferry.home;

import android.content.Context;
import android.content.SharedPreferences;

final class HomePrefs {
    static final String PREFS = "deskferry_home";
    static final String PREF_RELAY_URL = "relay_url";
    static final String PREF_LOCAL_PORT = "local_port";
    static final String PREF_PROXY = "proxy";
    static final int DEFAULT_LOCAL_PORT = 3389;

    private HomePrefs() {
    }

    static String loadRelayUrl(Context context) {
        return prefs(context).getString(PREF_RELAY_URL, RelayUrls.DEFAULT_RELAY_URL);
    }

    static int loadLocalPort(Context context) {
        return sanitizePort(prefs(context).getInt(PREF_LOCAL_PORT, DEFAULT_LOCAL_PORT));
    }

    static String loadProxy(Context context) {
        return prefs(context).getString(PREF_PROXY, ProxySettings.DEFAULT);
    }

    static void save(Context context, String relayUrl, int port, String proxy) {
        prefs(context)
                .edit()
                .putString(PREF_RELAY_URL, relayUrl)
                .putInt(PREF_LOCAL_PORT, sanitizePort(port))
                .putString(PREF_PROXY, proxy)
                .apply();
    }

    static int sanitizePort(int port) {
        return port > 0 && port < 65536 ? port : DEFAULT_LOCAL_PORT;
    }

    private static SharedPreferences prefs(Context context) {
        return context.getSharedPreferences(PREFS, Context.MODE_PRIVATE);
    }
}
