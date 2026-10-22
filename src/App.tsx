import classNames from "classnames";
import React, { useEffect } from "react";
import { BiLoader } from "react-icons/bi";
import { Link, Redirect, Route, Switch } from "react-router-dom";
import useFetch, { CachePolicies } from "use-http";
import { Map } from "./components/Map";
import { Caucasus } from "./dcs/maps/Caucasus";
import { DCSMap } from "./dcs/maps/DCSMap";
import { Marianas } from "./dcs/maps/Marianas";
import { PersianGulf } from "./dcs/maps/PersianGulf";
import { Sinai } from "./dcs/maps/Sinai";
import { Syria } from "./dcs/maps/Syria";
import { Falklands } from "./dcs/maps/Falklands";
import { Normandy } from "./dcs/maps/Normandy";
import { TheChannel } from "./dcs/maps/TheChannel";
import { Nevada } from "./dcs/maps/Nevada";
import { Kola } from "./dcs/maps/Kola";
import { Afghanistan } from "./dcs/maps/Afghanistan";
import { Iraq } from "./dcs/maps/Iraq";
import { GermanyCW } from "./dcs/maps/GermanyCW";
import { Server, serverStore } from "./stores/ServerStore";
import { route } from "./util";

type ServerMetadata = {
  name: string;
  players: Array<{ name: string; type: string }>;
};

function ServerOption({ server }: { server: ServerMetadata }) {
  return (
    <Link
      to={`/servers/${encodeURIComponent(server.name)}`}
      className="p-2 bg-gray-100 hover:bg-gray-200 border-gray-400 border rounded-sm shadow-sm w-full items-center flex flex-row"
    >
      <span className="text-3xl font-bold flex-grow">{server.name} </span>
      <span className="text-gray-600 text-sm font-light text-right">
        ({server.players.length} online)
      </span>
    </Link>
  );
}

function ServerConnectModal() {
  const {
    loading,
    error,
    data: servers,
    get,
  } = useFetch<Array<ServerMetadata>>(
    process.env.NODE_ENV === "production"
      ? `/api/servers`
      : `http://localhost:7789/api/servers`,
    []
  );

  return (
    <div
      className={classNames(
        "flex flex-col overflow-x-hidden overflow-y-auto absolute",
        "inset-0 z-50 bg-gray-100 mx-auto my-auto max-w-3xl",
        "border border-gray-200 rounded-sm shadow-md"
      )}
      style={{ maxHeight: "50%" }}
    >
      <div className="flex flex-row items-center p-2 border-b border-gray-400">
        <div className="text-2xl">Select Server</div>
      </div>
      <div className="flex flex-row p-2 h-full">
        {loading && (
          <BiLoader className="h-6 w-6 text-blue-400 animate-spin my-auto mx-auto" />
        )}
        {error && (
          <div>
            Something went wrong accessing the backend server. Please check your
            connection and <button onClick={() => get()}>try again</button>.
          </div>
        )}
        {servers && (
          <div className="flex flex-col gap-1 w-full">
            {servers.map((it) => (
              <ServerOption key={it.name} server={it} />
            ))}
          </div>
        )}
      </div>
    </div>
  );
}

// DCS server names often contain "#", "|" or spaces, so they are
// percent-encoded in URLs.
function decodeServerName(name: string): string {
  try {
    return decodeURIComponent(name);
  } catch {
    return name;
  }
}

function ServerContainer({ serverName }: { serverName: string }) {
  const [refLat, refLng] = serverStore((state) => {
    const globalObj = state.entities.get(0);
    if (!globalObj) return [undefined, undefined];
    return [
      globalObj.properties.ReferenceLatitude as number | undefined,
      globalObj.properties.ReferenceLongitude as number | undefined,
    ];
  });

  const {
    response,
    data: server,
    loading,
    error,
  } = useFetch<Server>(
    route(`/servers/${encodeURIComponent(serverName)}`),
    { cachePolicy: CachePolicies.NO_CACHE },
    [serverName]
  );

  useEffect(() => {
    if (server && !error && !loading) {
      serverStore.setState({ server: server });
      return () => serverStore.setState({ server: null });
    }
  }, [server, error, loading]);

  if (response.status === 404) {
    return <Redirect to="/" />;
  }

  if (error) {
    return (
      <div className="p-2 border border-red-400 bg-red-100 text-red-400">
        Error: {error.toString()}
      </div>
    );
  }

  if (loading || refLat === undefined || refLng === undefined) {
    return (
      <BiLoader className="h-6 w-6 text-blue-400 animate-spin my-auto mx-auto" />
    );
  }

  // Tacview's ReferenceLatitude/Longitude is the DCS theater origin (x=0, z=0),
  // possibly rounded. Origins: Caucasus 45.1/34.3, Sinai 30.0/31.2,
  // Syria 35.0/35.9, PersianGulf 26.2/56.2, Iraq 33.3/44.2, Marianas 13.5/144.8,
  // Falklands -52.5/-59.2, Normandy 49.5/-0.3, TheChannel 50.9/1.6,
  // Nevada 39.8/-114.7, Kola 68.0/22.5, Afghanistan 33.9/66.2, GermanyCW 54.7/20.5
  let dcsMap: DCSMap | null = null;
  const lat = Number(refLat);
  const lng = Number(refLng);
  if (lat >= 38 && lat <= 48 && lng >= 26 && lng <= 48) {
    dcsMap = Caucasus;
  } else if (lat >= 25 && lat < 34 && lng >= 27 && lng <= 37) {
    dcsMap = Sinai;
  } else if (lat >= 28 && lat < 38 && lng >= 27 && lng <= 42) {
    dcsMap = Syria;
  } else if (lat >= 31 && lat <= 36 && lng > 42 && lng <= 46) {
    dcsMap = Iraq;
  } else if (lat >= 20 && lat <= 33 && lng >= 46 && lng <= 64) {
    dcsMap = PersianGulf;
  } else if (lat >= 7 && lat <= 23 && lng >= 136 && lng <= 153) {
    dcsMap = Marianas;
  } else if (lat >= -59 && lat <= -45 && lng >= -88 && lng <= -38) {
    dcsMap = Falklands;
  } else if (lat >= 50 && lat <= 52 && lng >= 1 && lng <= 4) {
    dcsMap = TheChannel;
  } else if (lat >= 48 && lat <= 52 && lng >= -4 && lng <= 4) {
    dcsMap = Normandy;
  } else if (lat >= 32 && lat <= 40 && lng >= -122 && lng <= -112) {
    dcsMap = Nevada;
  } else if (lat >= 62 && lat <= 73 && lng >= -4 && lng <= 40) {
    dcsMap = Kola;
  } else if (lat >= 28 && lat <= 39 && lng >= 60 && lng <= 74) {
    dcsMap = Afghanistan;
  } else if (lat >= 52 && lat <= 57 && lng >= 17 && lng <= 24) {
    dcsMap = GermanyCW;
  } else {
    console.log(refLat, refLng);
    return (
      <div className="p-2 border border-red-400 bg-red-100 text-red-400">
        Failed to detect map. Please include the following in a bug report: (
        {refLat}, {refLng})
      </div>
    );
  }

  return <Map dcsMap={dcsMap} />;
}

function App() {
  return (
    <div className="bg-gray-700 max-w-full max-h-full w-full h-full">
      <Switch>
        <Route exact path="/" component={ServerConnectModal} />
        <Route
          exact
          path="/servers/:serverName"
          render={({
            match: {
              params: { serverName },
            },
          }) => {
            return <ServerContainer serverName={decodeServerName(serverName)} />;
          }}
        />
      </Switch>
    </div>
  );
}

export default App;
